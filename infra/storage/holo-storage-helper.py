#!/usr/bin/env python3
"""Restricted root helper for Holo storage-pool filesystems and mount roots."""

import argparse
import fcntl
import json
import os
import re
import selectors
import stat
import subprocess
import sys
import time
from contextlib import contextmanager
from pathlib import Path


MAX_CONFIG_BYTES = 16 * 1024
MAX_COMMAND_OUTPUT = 256 * 1024
COMMAND_TIMEOUT_SECONDS = 15
POOL_ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
DEVICE_RE = re.compile(r"^/dev/[A-Za-z0-9._-]+$")
DIRECTORY_FLAGS = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
FILE_FLAGS = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
WRITE_FLAGS = os.O_RDWR | os.O_CREAT | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)


class HelperError(RuntimeError):
    def __init__(self, reason):
        super().__init__(reason)
        self.reason = reason


def _required_command(name):
    paths = {
        "blkid": ("/usr/sbin/blkid", "/sbin/blkid", "/usr/bin/blkid", "/bin/blkid"),
        "findmnt": ("/usr/bin/findmnt", "/usr/sbin/findmnt", "/bin/findmnt", "/sbin/findmnt"),
        "lsblk": ("/usr/bin/lsblk", "/usr/sbin/lsblk", "/bin/lsblk", "/sbin/lsblk"),
        "mkfs.xfs": ("/usr/sbin/mkfs.xfs", "/sbin/mkfs.xfs", "/usr/bin/mkfs.xfs", "/bin/mkfs.xfs"),
        "mount": ("/usr/bin/mount", "/usr/sbin/mount", "/bin/mount", "/sbin/mount"),
        "umount": ("/usr/bin/umount", "/usr/sbin/umount", "/bin/umount", "/sbin/umount"),
    }
    for path in paths.get(name, ()):
        try:
            info = os.stat(path)
        except OSError:
            continue
        if stat.S_ISREG(info.st_mode) and info.st_uid == 0 and not info.st_mode & 0o022 and os.access(path, os.X_OK):
            return path
    raise HelperError("command_unavailable")


def _validate_directory_stat(fd, root_owner_uids):
    info = os.fstat(fd)
    mode = stat.S_IMODE(info.st_mode)
    if not stat.S_ISDIR(info.st_mode) or info.st_uid not in root_owner_uids:
        raise HelperError("untrusted_parent_directory")
    if mode & 0o022 and not (info.st_uid == 0 and mode & stat.S_ISVTX):
        raise HelperError("untrusted_parent_directory")


def _open_controlled_directory(path, root_owner_uids):
    if not isinstance(path, str) or not os.path.isabs(path) or os.path.normpath(path) != path:
        raise HelperError("invalid_controlled_path")
    try:
        fd = os.open("/", DIRECTORY_FLAGS)
    except OSError:
        raise HelperError("path_unavailable")
    try:
        for component in Path(path).parts[1:]:
            try:
                child = os.open(component, DIRECTORY_FLAGS, dir_fd=fd)
            except OSError:
                raise HelperError("path_unavailable")
            os.close(fd)
            fd = child
            _validate_directory_stat(fd, root_owner_uids)
        return fd
    except Exception:
        os.close(fd)
        raise


def _open_or_create_controlled_directory(path, root_owner_uids, mode=0o755):
    parent, name = os.path.split(path.rstrip("/"))
    if not name:
        raise HelperError("invalid_controlled_path")
    parent_fd = _open_controlled_directory(parent, root_owner_uids)
    try:
        try:
            os.mkdir(name, mode, dir_fd=parent_fd)
        except FileExistsError:
            pass
        try:
            fd = os.open(name, DIRECTORY_FLAGS, dir_fd=parent_fd)
        except OSError:
            raise HelperError("path_unavailable")
    finally:
        os.close(parent_fd)
    try:
        _validate_directory_stat(fd, root_owner_uids)
        info = os.fstat(fd)
        if info.st_uid == 0 and stat.S_IMODE(info.st_mode) != mode:
            raise HelperError("untrusted_parent_directory")
        return fd
    except Exception:
        os.close(fd)
        raise


def load_config(path, root_owner_uids=(0,)):
    """Load the root-owned install config without following path symlinks."""
    parent, name = os.path.split(path)
    parent_fd = _open_controlled_directory(parent, root_owner_uids)
    try:
        try:
            fd = os.open(name, FILE_FLAGS, dir_fd=parent_fd)
        except OSError:
            raise HelperError("config_unavailable")
    finally:
        os.close(parent_fd)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid not in root_owner_uids:
            raise HelperError("config_untrusted")
        if info.st_mode & 0o077 or info.st_size <= 0 or info.st_size > MAX_CONFIG_BYTES:
            raise HelperError("config_untrusted")
        with os.fdopen(os.dup(fd), "rb") as stream:
            raw = stream.read(MAX_CONFIG_BYTES + 1)
        if len(raw) > MAX_CONFIG_BYTES:
            raise HelperError("config_untrusted")
        try:
            value = json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError):
            raise HelperError("config_invalid")
        if not isinstance(value, dict):
            raise HelperError("config_invalid")
        return value
    finally:
        os.close(fd)


class StoragePrivilegeHelper:
    def __init__(self, config, runner=None, root_owner_uids=(0,)):
        if not isinstance(config, dict):
            raise HelperError("config_invalid")
        base = config.get("storage_pool_root_base")
        data_dir = config.get("data_dir")
        service_uid = config.get("service_uid")
        service_gid = config.get("service_gid")
        runtime_dir = config.get("runtime_dir", "/run/holo-privileged")
        for value in (base, data_dir, runtime_dir):
            if not isinstance(value, str) or not os.path.isabs(value) or os.path.normpath(value) != value:
                raise HelperError("config_invalid")
        if base != os.path.join(data_dir, "storage-pools"):
            raise HelperError("config_invalid")
        if type(service_uid) is not int or service_uid <= 0 or type(service_gid) is not int or service_gid <= 0:
            raise HelperError("config_invalid")
        self.storage_pool_root_base = base
        self.data_dir = data_dir
        self.service_uid = service_uid
        self.service_gid = service_gid
        self.runtime_dir = runtime_dir
        self.runner = runner
        self.root_owner_uids = frozenset(root_owner_uids)

    def execute(self, argv):
        if not isinstance(argv, (list, tuple)) or not argv or any(not isinstance(arg, str) or "\x00" in arg for arg in argv):
            raise HelperError("invalid_arguments")
        command = argv[0]
        args = list(argv[1:])
        if command in ("mkdir", "chown", "mkfs.xfs", "mount", "umount", "prepare-directories"):
            with self._exclusive_lock():
                if command == "mkdir":
                    self._mkdir(args)
                elif command == "chown":
                    self._chown(args)
                elif command == "mkfs.xfs":
                    self._format_xfs(args)
                elif command == "mount":
                    self._mount(args)
                elif command == "umount":
                    self._umount(args)
                else:
                    if args:
                        raise HelperError("invalid_arguments")
                    self._prepare_directories()
            return ""
        if command == "lsblk":
            return self._lsblk(args)
        if command == "findmnt":
            return self._findmnt(args)
        raise HelperError("unsupported_command")

    def pool_id_for_path(self, path):
        if not isinstance(path, str) or not os.path.isabs(path) or os.path.normpath(path) != path:
            raise HelperError("invalid_pool_path")
        parent, pool_id = os.path.split(path)
        if parent != self.storage_pool_root_base or not POOL_ID_RE.fullmatch(pool_id) or ".." in pool_id:
            raise HelperError("invalid_pool_path")
        return pool_id

    def open_pool_root(self, pool_id):
        if not isinstance(pool_id, str) or pool_id in (".", "..") or not POOL_ID_RE.fullmatch(pool_id) or ".." in pool_id:
            raise HelperError("invalid_pool_id")
        base_fd = self._open_controlled_directory(self.storage_pool_root_base)
        try:
            try:
                fd = os.open(pool_id, DIRECTORY_FLAGS, dir_fd=base_fd)
            except OSError:
                raise HelperError("pool_root_unavailable")
            return fd
        finally:
            os.close(base_fd)

    def _open_controlled_directory(self, path):
        return _open_controlled_directory(path, self.root_owner_uids)

    @contextmanager
    def _exclusive_lock(self):
        lock_dir_fd = _open_or_create_controlled_directory(self.runtime_dir, self.root_owner_uids)
        try:
            created = False
            try:
                fd = os.open("lock", WRITE_FLAGS | os.O_EXCL, 0o600, dir_fd=lock_dir_fd)
                created = True
            except FileExistsError:
                try:
                    fd = os.open("lock", WRITE_FLAGS, dir_fd=lock_dir_fd)
                except OSError:
                    raise HelperError("mutation_lock_unavailable")
            except OSError:
                raise HelperError("mutation_lock_unavailable")
        finally:
            os.close(lock_dir_fd)
        try:
            if created:
                os.fchmod(fd, 0o600)
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid not in self.root_owner_uids or info.st_mode & 0o077:
                raise HelperError("mutation_lock_untrusted")
            deadline = time.monotonic() + 10
            while True:
                try:
                    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    break
                except BlockingIOError:
                    if time.monotonic() >= deadline:
                        raise HelperError("mutation_lock_busy")
                    time.sleep(0.05)
            yield
        finally:
            os.close(fd)

    def _prepare_directories(self):
        data_fd = self._open_controlled_directory(self.data_dir)
        try:
            os.fchown(data_fd, 0, self.service_gid)
            os.fchmod(data_fd, 0o1770)
            self._prepare_metadata_files(data_fd)
            pool_base_fd = self._open_child_directory(data_fd, "storage-pools", create=True, mode=0o755)
            try:
                os.fchown(pool_base_fd, 0, 0)
                os.fchmod(pool_base_fd, 0o755)
                for child_name in sorted(os.listdir(pool_base_fd)):
                    if not POOL_ID_RE.fullmatch(child_name) or ".." in child_name:
                        raise HelperError("unexpected_pool_root_entry")
                    try:
                        child_fd = os.open(child_name, DIRECTORY_FLAGS, dir_fd=pool_base_fd)
                    except OSError:
                        raise HelperError("untrusted_pool_root_entry")
                    try:
                        mount_state = self._pool_mount_state(child_fd)
                        if mount_state is None:
                            owner = (0, 0)
                            mode = 0o755
                        elif mount_state[1] == "xfs" and mount_state[0] == self._device_number(os.fstat(child_fd).st_dev):
                            owner = (self.service_uid, self.service_gid)
                            mode = 0o750
                        else:
                            raise HelperError("unsupported_pool_mount")
                        os.fchown(child_fd, *owner)
                        os.fchmod(child_fd, mode)
                    finally:
                        os.close(child_fd)
            finally:
                os.close(pool_base_fd)

            for name in ("targets", "media-state"):
                child_fd = self._open_child_directory(data_fd, name, create=True, mode=0o750)
                try:
                    os.fchown(child_fd, self.service_uid, self.service_gid)
                    os.fchmod(child_fd, 0o750)
                finally:
                    os.close(child_fd)
        except OSError:
            raise HelperError("directory_preparation_failed")
        finally:
            os.close(data_fd)

    def _prepare_metadata_files(self, data_fd):
        for name in ("holo.db", "holo.db-wal", "holo.db-shm", "holo.db-journal"):
            try:
                fd = os.open(name, FILE_FLAGS, dir_fd=data_fd)
            except FileNotFoundError:
                continue
            except OSError:
                raise HelperError("metadata_file_untrusted")
            try:
                info = os.fstat(fd)
                if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid not in (0, self.service_uid):
                    raise HelperError("metadata_file_untrusted")
                os.fchown(fd, self.service_uid, self.service_gid)
                os.fchmod(fd, 0o600)
            except OSError:
                raise HelperError("metadata_file_update_failed")
            finally:
                os.close(fd)

    def _open_child_directory(self, parent_fd, name, create=False, mode=0o750):
        if "/" in name or name in ("", ".", ".."):
            raise HelperError("invalid_directory_name")
        if create:
            try:
                os.mkdir(name, mode, dir_fd=parent_fd)
            except FileExistsError:
                pass
            except OSError:
                raise HelperError("directory_preparation_failed")
        try:
            fd = os.open(name, DIRECTORY_FLAGS, dir_fd=parent_fd)
        except OSError:
            raise HelperError("untrusted_directory")
        return fd

    def _mkdir(self, args):
        if len(args) != 2 or args[0] != "-p":
            raise HelperError("invalid_arguments")
        pool_id = self.pool_id_for_path(args[1])
        base_fd = self._open_controlled_directory(self.storage_pool_root_base)
        try:
            try:
                os.mkdir(pool_id, 0o755, dir_fd=base_fd)
            except FileExistsError:
                pass
            except OSError:
                raise HelperError("pool_root_create_failed")
            try:
                pool_fd = os.open(pool_id, DIRECTORY_FLAGS, dir_fd=base_fd)
            except OSError:
                raise HelperError("pool_root_unavailable")
            try:
                info = os.fstat(pool_fd)
                if not stat.S_ISDIR(info.st_mode):
                    raise HelperError("pool_root_unavailable")
                if info.st_uid not in self.root_owner_uids and not self._is_pool_mount(pool_fd):
                    raise HelperError("pool_root_untrusted")
            finally:
                os.close(pool_fd)
        finally:
            os.close(base_fd)

    def _chown(self, args):
        if len(args) != 2 or args[0] != f"{self.service_uid}:{self.service_gid}":
            raise HelperError("invalid_owner")
        pool_id = self.pool_id_for_path(args[1])
        fd = self.open_pool_root(pool_id)
        try:
            if not self._is_pool_mount(fd):
                raise HelperError("pool_root_not_mounted")
            os.fchown(fd, self.service_uid, self.service_gid)
        except OSError:
            raise HelperError("ownership_update_failed")
        finally:
            os.close(fd)

    def _open_block_device(self, path):
        if not isinstance(path, str) or not DEVICE_RE.fullmatch(path):
            raise HelperError("invalid_device")
        try:
            fd = os.open(path, FILE_FLAGS | getattr(os, "O_NONBLOCK", 0))
        except OSError:
            raise HelperError("device_unavailable")
        if not stat.S_ISBLK(os.fstat(fd).st_mode):
            os.close(fd)
            raise HelperError("not_block_device")
        return fd

    def _device_tree(self, fd):
        info = os.fstat(fd)
        fd_path = f"/proc/self/fd/{fd}"
        payload = self._run("lsblk", ["-J", "-b", "-p", "-o", "NAME,TYPE,FSTYPE,PTTYPE,MOUNTPOINTS,MAJ:MIN,RO", fd_path], pass_fds=(fd,))
        try:
            blockdevices = json.loads(payload).get("blockdevices", [])
        except (json.JSONDecodeError, AttributeError):
            raise HelperError("invalid_device_inventory")
        if not isinstance(blockdevices, list):
            raise HelperError("invalid_device_inventory")
        expected = f"{os.major(info.st_rdev)}:{os.minor(info.st_rdev)}"
        matches = []

        def visit(items):
            for node in items:
                if not isinstance(node, dict):
                    raise HelperError("invalid_device_inventory")
                if node.get("maj:min") == expected:
                    matches.append(node)
                children = node.get("children") or []
                if not isinstance(children, list):
                    raise HelperError("invalid_device_inventory")
                visit(children)

        visit(blockdevices)
        if len(matches) != 1:
            raise HelperError("device_identity_mismatch")
        return matches[0]

    def _verify_unused_whole_disk(self, fd, formatting):
        node = self._device_tree(fd)
        children = node.get("children") or []
        readonly = node.get("ro", False)
        if isinstance(readonly, str):
            readonly = readonly not in ("", "0", "false", "False")
        if node.get("type") != "disk" or children or readonly:
            raise HelperError("device_not_unused_whole_disk")
        mountpoints = node.get("mountpoints", node.get("mountpoint"))
        if mountpoints not in (None, [], [None], ""):
            raise HelperError("device_in_use")
        info = os.fstat(fd)
        device_id = f"{os.major(info.st_rdev)}:{os.minor(info.st_rdev)}"
        try:
            root_info = os.stat("/")
            if f"{os.major(root_info.st_dev)}:{os.minor(root_info.st_dev)}" == device_id:
                raise HelperError("system_device")
        except OSError:
            raise HelperError("device_safety_check_failed")
        if self._device_is_mounted_or_swap(device_id):
            raise HelperError("device_in_use")
        holders = f"/sys/dev/block/{device_id}/holders"
        try:
            if os.listdir(holders):
                raise HelperError("device_in_use")
        except FileNotFoundError:
            raise HelperError("device_safety_check_failed")
        except OSError:
            raise HelperError("device_safety_check_failed")

        if formatting:
            if node.get("fstype") or node.get("pttype"):
                raise HelperError("device_has_existing_signature")
            result = self._run_status("blkid", ["-p", "-o", "export", f"/proc/self/fd/{fd}"], pass_fds=(fd,), allowed=(2,))
            if result != 2:
                raise HelperError("device_has_existing_signature")
        elif str(node.get("fstype", "")).lower() != "xfs" or node.get("pttype"):
            raise HelperError("unsupported_filesystem")

    def _device_is_mounted_or_swap(self, device_id):
        try:
            with open("/proc/self/mountinfo", "r", encoding="utf-8") as source:
                for line in source:
                    fields = line.split()
                    if len(fields) > 2 and fields[2] == device_id:
                        return True
            with open("/proc/swaps", "r", encoding="utf-8") as source:
                next(source, None)
                for line in source:
                    fields = line.split()
                    if not fields:
                        continue
                    try:
                        info = os.stat(fields[0])
                    except OSError:
                        continue
                    if stat.S_ISBLK(info.st_mode) and f"{os.major(info.st_rdev)}:{os.minor(info.st_rdev)}" == device_id:
                        return True
        except OSError:
            raise HelperError("device_safety_check_failed")
        return False

    def _format_xfs(self, args):
        if len(args) != 2 or args[0] != "-f":
            raise HelperError("invalid_arguments")
        fd = self._open_block_device(args[1])
        try:
            self._verify_unused_whole_disk(fd, formatting=True)
            self._run("mkfs.xfs", ["-f", f"/proc/self/fd/{fd}"], pass_fds=(fd,))
        finally:
            os.close(fd)

    def _mount(self, args):
        if len(args) != 4 or args[:2] != ["-o", "noatime,nodiratime"]:
            raise HelperError("invalid_arguments")
        device_path, pool_path = args[2], args[3]
        pool_id = self.pool_id_for_path(pool_path)
        target_fd = self.open_pool_root(pool_id)
        try:
            target_info = os.fstat(target_fd)
            mounted_pool = self._is_pool_mount(target_fd)
            if target_info.st_uid != 0 and not mounted_pool:
                raise HelperError("pool_root_untrusted")
            device_fd = self._open_block_device(device_path)
            try:
                source_targets = self._query("findmnt", ["-rn", "-S", f"/proc/self/fd/{device_fd}", "-o", "TARGET"], pass_fds=(device_fd,)).strip()
                target_source = self._query("findmnt", ["-rn", "-M", f"/proc/self/fd/{target_fd}", "-o", "SOURCE"], pass_fds=(target_fd,)).strip()
                if target_source:
                    if source_targets and os.path.normpath(source_targets) == pool_path and self._same_device(target_source, device_fd) and mounted_pool:
                        return
                    raise HelperError("pool_root_already_mounted")
                if source_targets:
                    raise HelperError("device_already_mounted")
                if target_info.st_uid != 0:
                    raise HelperError("pool_root_untrusted")
                self._verify_unused_whole_disk(device_fd, formatting=False)
                self._run(
                    "mount",
                    ["-o", "noatime,nodiratime", f"/proc/self/fd/{device_fd}", f"/proc/self/fd/{target_fd}"],
                    pass_fds=(device_fd, target_fd),
                )
            finally:
                os.close(device_fd)
        finally:
            os.close(target_fd)

    def _umount(self, args):
        if len(args) != 2:
            raise HelperError("invalid_arguments")
        pool_path, expected_device = args
        pool_id = self.pool_id_for_path(pool_path)
        target_fd = self.open_pool_root(pool_id)
        device_fd = self._open_block_device(expected_device)
        try:
            source = self._query("findmnt", ["-rn", "-M", f"/proc/self/fd/{target_fd}", "-o", "SOURCE"], pass_fds=(target_fd,)).strip()
            if not source:
                return
            if not self._same_device(source, device_fd):
                raise HelperError("pool_mount_source_mismatch")
        finally:
            os.close(device_fd)
            os.close(target_fd)
        self._run("umount", [pool_path])

    def _lsblk(self, args):
        if len(args) != 3 or args[:2] != ["-no", "FSTYPE"]:
            raise HelperError("invalid_arguments")
        fd = self._open_block_device(args[2])
        try:
            return self._run("lsblk", ["-no", "FSTYPE", f"/proc/self/fd/{fd}"], pass_fds=(fd,))
        finally:
            os.close(fd)

    def _findmnt(self, args):
        if len(args) != 5 or args[0] != "-rn" or args[3] != "-o":
            raise HelperError("invalid_arguments")
        if args[1] == "-S" and args[4] == "TARGET":
            fd = self._open_block_device(args[2])
            try:
                return self._query("findmnt", ["-rn", "-S", f"/proc/self/fd/{fd}", "-o", "TARGET"], pass_fds=(fd,))
            finally:
                os.close(fd)
        if args[1] == "-M" and args[4] == "SOURCE":
            pool_id = self.pool_id_for_path(args[2])
            fd = self.open_pool_root(pool_id)
            try:
                return self._query("findmnt", ["-rn", "-M", f"/proc/self/fd/{fd}", "-o", "SOURCE"], pass_fds=(fd,))
            finally:
                os.close(fd)
        raise HelperError("invalid_arguments")

    def _is_pool_mount(self, fd):
        try:
            state = self._pool_mount_state(fd)
            if state is None or state[1] != "xfs":
                return False
            info = os.fstat(fd)
            return state[0] == self._device_number(info.st_dev)
        except HelperError:
            return False

    def _pool_mount_state(self, fd):
        value = self._query(
            "findmnt",
            ["-rn", "-M", f"/proc/self/fd/{fd}", "-o", "MAJ:MIN,FSTYPE"],
            pass_fds=(fd,),
        ).strip()
        if not value:
            return None
        fields = value.split()
        if len(fields) != 2 or not re.fullmatch(r"[0-9]+:[0-9]+", fields[0]) or not re.fullmatch(r"[A-Za-z0-9_-]+", fields[1]):
            raise HelperError("invalid_mount_inventory")
        return fields[0], fields[1].lower()

    @staticmethod
    def _device_number(device):
        return f"{os.major(device)}:{os.minor(device)}"

    def _same_device(self, source, expected_fd):
        try:
            source_info = os.stat(source)
            expected_info = os.fstat(expected_fd)
            return stat.S_ISBLK(source_info.st_mode) and stat.S_ISBLK(expected_info.st_mode) and source_info.st_rdev == expected_info.st_rdev
        except OSError:
            return False

    def _run(self, command, args, pass_fds=()):
        if self.runner is not None:
            return self.runner(command, args)
        return _run_system(command, args, pass_fds=pass_fds)

    def _query(self, command, args, pass_fds=()):
        if self.runner is not None:
            return self.runner(command, args)
        return _run_system(command, args, pass_fds=pass_fds, allowed=(1,))

    def _run_status(self, command, args, pass_fds=(), allowed=()):
        if self.runner is not None:
            output = self.runner(command, args)
            return 2 if not output else 0
        return _run_system_status(command, args, pass_fds=pass_fds, allowed=allowed)


def _run_system_result(command, args, pass_fds=(), allowed=()):
    process = None
    selector = selectors.DefaultSelector()
    output = bytearray()
    try:
        process = subprocess.Popen(
            [_required_command(command), *args],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            close_fds=True,
            pass_fds=tuple(pass_fds),
            env={"PATH": "/usr/sbin:/sbin:/usr/bin:/bin", "LC_ALL": "C"},
        )
    except OSError:
        raise HelperError("command_failed")
    deadline = time.monotonic() + COMMAND_TIMEOUT_SECONDS
    try:
        for stream in (process.stdout, process.stderr):
            os.set_blocking(stream.fileno(), False)
            selector.register(stream, selectors.EVENT_READ)
        while selector.get_map():
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise HelperError("command_timeout")
            for key, _ in selector.select(min(remaining, 0.25)):
                chunk = os.read(key.fd, min(65536, MAX_COMMAND_OUTPUT + 1 - len(output)))
                if not chunk:
                    selector.unregister(key.fileobj)
                    key.fileobj.close()
                    continue
                output.extend(chunk)
                if len(output) > MAX_COMMAND_OUTPUT:
                    raise HelperError("command_output_too_large")
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise HelperError("command_timeout")
        try:
            returncode = process.wait(timeout=remaining)
        except subprocess.TimeoutExpired:
            raise HelperError("command_timeout")
        if returncode != 0 and returncode not in allowed:
            raise HelperError("command_failed")
        try:
            return bytes(output).decode("utf-8"), returncode
        except UnicodeDecodeError:
            raise HelperError("invalid_command_output")
    finally:
        selector.close()
        if process.poll() is None:
            process.kill()
            process.wait()
        for stream in (process.stdout, process.stderr):
            if stream is not None and not stream.closed:
                stream.close()


def _run_system(command, args, pass_fds=(), allowed=()):
    output, _ = _run_system_result(command, args, pass_fds=pass_fds, allowed=allowed)
    return output


def _run_system_status(command, args, pass_fds=(), allowed=()):
    _, returncode = _run_system_result(command, args, pass_fds=pass_fds, allowed=allowed)
    return returncode


def main(argv=None):
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument("--config", required=True)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args(argv)
    try:
        if os.geteuid() != 0:
            raise HelperError("root_required")
        config = load_config(args.config)
        helper = StoragePrivilegeHelper(config)
        output = helper.execute(args.command)
        if output:
            sys.stdout.write(output)
        return 0
    except HelperError as exc:
        sys.stderr.write(f"holo-storage-helper: {exc.reason}\n")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
