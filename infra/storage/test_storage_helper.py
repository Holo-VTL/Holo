import importlib.util
import builtins
import io
import json
import os
import stat
import tempfile
import unittest
from types import SimpleNamespace
from pathlib import Path
from unittest.mock import patch


HELPER_PATH = Path(__file__).with_name("holo-storage-helper.py")
spec = importlib.util.spec_from_file_location("holo_storage_helper", HELPER_PATH)
helper_module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper_module)
StoragePrivilegeHelper = helper_module.StoragePrivilegeHelper
HelperError = helper_module.HelperError


class FakeCommandRunner:
    def __init__(self, lsblk_payload=None):
        self.calls = []
        self.lsblk_payload = lsblk_payload or {"blockdevices": []}

    def __call__(self, executable, args):
        self.calls.append((executable, list(args)))
        if Path(executable).name == "lsblk":
            return json.dumps(self.lsblk_payload)
        return ""


class StorageHelperTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="holo-storage-helper-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(os.path.realpath(self.temporary.name))
        self.data_dir = self.root / "data"
        self.data_dir.mkdir(mode=0o700)
        self.runtime_dir = self.root / "run"
        self.runtime_dir.mkdir(mode=0o700)
        self.pool_base = self.data_dir / "storage-pools"
        self.pool_base.mkdir(mode=0o755)
        self.runner = FakeCommandRunner()
        self.helper = StoragePrivilegeHelper(
            {
                "storage_pool_root_base": str(self.pool_base),
                "data_dir": str(self.data_dir),
                "runtime_dir": str(self.runtime_dir),
                "service_uid": os.getuid(),
                "service_gid": os.getgid(),
            },
            runner=self.runner,
            root_owner_uids={0, os.getuid()},
        )

    def test_accepts_only_one_canonical_pool_root_directly_under_base(self):
        expected = self.pool_base / "pool-a"
        self.assertEqual(self.helper.pool_id_for_path(str(expected)), "pool-a")
        for path in (
            str(self.pool_base / "pool-a" / "nested"),
            str(self.root / "storage-pools-escape" / "pool-a"),
            str(self.pool_base / ".." / "outside"),
            str(self.pool_base / "bad..pool"),
        ):
            with self.subTest(path=path), self.assertRaises(HelperError):
                self.helper.pool_id_for_path(path)

    def test_mkdir_creates_only_a_direct_child_and_rejects_symlink(self):
        pool_root = self.pool_base / "pool-a"
        self.helper.execute(["mkdir", "-p", str(pool_root)])
        self.assertTrue(pool_root.is_dir())
        self.assertEqual(stat.S_IMODE(pool_root.stat().st_mode), 0o755)

        outside = self.root / "outside"
        outside.mkdir()
        link = self.pool_base / "pool-link"
        link.symlink_to(outside, target_is_directory=True)
        with self.assertRaises(HelperError):
            self.helper.execute(["mkdir", "-p", str(link)])
        self.assertEqual(list(outside.iterdir()), [])

    def test_rejects_group_or_world_writable_storage_base(self):
        self.pool_base.chmod(0o777)
        with self.assertRaises(HelperError):
            self.helper.execute(["mkdir", "-p", str(self.pool_base / "pool-a")])

    def test_fchown_uses_the_opened_pool_root_after_path_replacement(self):
        pool_root = self.pool_base / "pool-a"
        pool_root.mkdir(mode=0o755)
        original_stat = pool_root.stat()
        original_open = self.helper.open_pool_root
        opened_identity = {}

        def replace_after_open(pool_id):
            fd = original_open(pool_id)
            opened = os.fstat(fd)
            opened_identity["inode"] = (opened.st_dev, opened.st_ino)
            pool_root.rename(self.pool_base / "pool-a-original")
            pool_root.mkdir(mode=0o755)
            return fd

        self.helper.open_pool_root = replace_after_open
        opened_fd_identity = {}

        def capture_fchown(fd, uid, gid):
            opened = os.fstat(fd)
            opened_fd_identity["inode"] = (opened.st_dev, opened.st_ino)

        with patch.object(helper_module.os, "fchown", side_effect=capture_fchown):
            with patch.object(self.helper, "_is_pool_mount", return_value=True):
                self.helper.execute(["chown", f"{os.getuid()}:{os.getgid()}", str(pool_root)])
        self.assertEqual(opened_identity["inode"], (original_stat.st_dev, original_stat.st_ino))
        self.assertEqual(opened_fd_identity["inode"], opened_identity["inode"])
        self.assertNotEqual(opened_fd_identity["inode"], (pool_root.stat().st_dev, pool_root.stat().st_ino))

    def test_owner_is_fixed_by_config_not_caller_environment_or_argv(self):
        pool_root = self.pool_base / "pool-a"
        pool_root.mkdir(mode=0o755)
        with patch.dict(os.environ, {"HOLO_STORAGE_SERVICE_UID": "0", "HOLO_STORAGE_POOL_ROOT_BASE": "/"}):
            with self.assertRaises(HelperError):
                self.helper.execute(["chown", "0:0", str(pool_root)])
            with patch.object(self.helper, "_is_pool_mount", return_value=True):
                self.helper.execute(["chown", f"{os.getuid()}:{os.getgid()}", str(pool_root)])

    def test_rejects_device_paths_outside_a_single_dev_component(self):
        for command in (
            ["mkfs.xfs", "-f", "/dev/../etc/passwd"],
            ["mount", "-o", "noatime,nodiratime", "/dev/mapper/pool", str(self.pool_base / "pool-a")],
        ):
            with self.subTest(command=command), self.assertRaises(HelperError):
                self.helper.execute(command)
        self.assertEqual(self.runner.calls, [])

    def test_rejects_symlinked_parent_and_symlinked_runtime_directory(self):
        parent_link = self.root / "pool-base-link"
        parent_link.symlink_to(self.pool_base, target_is_directory=True)
        with self.assertRaises(HelperError):
            helper_module.StoragePrivilegeHelper(
                {
                    "storage_pool_root_base": str(parent_link),
                    "runtime_dir": str(self.runtime_dir),
                    "service_uid": os.getuid(),
                    "service_gid": os.getgid(),
                },
                runner=self.runner,
                root_owner_uids={0, os.getuid()},
            ).execute(["mkdir", "-p", str(parent_link / "pool-a")])

        original_runtime = self.runtime_dir
        original_runtime.rename(self.data_dir / "runtime-real")
        original_runtime.symlink_to(self.data_dir / "runtime-real", target_is_directory=True)
        with self.assertRaises(HelperError):
            self.helper.execute(["mkdir", "-p", str(self.pool_base / "pool-a")])

    def test_rejects_invalid_fixed_owner_configuration(self):
        for field, value in (("service_uid", 0), ("service_gid", -1), ("service_uid", "1000")):
            config = {
                "storage_pool_root_base": str(self.pool_base),
                "runtime_dir": str(self.runtime_dir),
                "service_uid": os.getuid(),
                "service_gid": os.getgid(),
            }
            config[field] = value
            with self.subTest(field=field, value=value), self.assertRaises(HelperError):
                StoragePrivilegeHelper(config, runner=self.runner)

    def test_load_config_rejects_symlinked_parent(self):
        real_config_dir = self.root / "config-real"
        real_config_dir.mkdir(mode=0o700)
        config_file = real_config_dir / "storage.json"
        config_file.write_text(json.dumps({"storage_pool_root_base": str(self.pool_base)}), encoding="utf-8")
        config_file.chmod(0o600)
        config_link = self.root / "config-link"
        config_link.symlink_to(real_config_dir, target_is_directory=True)

        with self.assertRaises(HelperError):
            helper_module.load_config(str(config_link / "storage.json"), root_owner_uids={0, os.getuid()})

    def test_refuses_partition_and_system_disk_during_format_validation(self):
        fixture = self.root / "not-a-real-device"
        fixture.write_bytes(b"")
        fd = os.open(fixture, os.O_RDONLY)
        self.addCleanup(os.close, fd)

        with patch.object(self.helper, "_device_tree", return_value={"type": "part", "children": []}):
            with self.assertRaises(HelperError):
                self.helper._verify_unused_whole_disk(fd, formatting=True)

        root_device = os.stat("/").st_dev
        fake_info = SimpleNamespace(st_rdev=root_device)
        with patch.object(self.helper, "_device_tree", return_value={"type": "disk", "children": [], "ro": False, "mountpoints": None}):
            with patch.object(helper_module.os, "fstat", return_value=fake_info):
                with self.assertRaises(HelperError):
                    self.helper._verify_unused_whole_disk(fd, formatting=True)

    def test_prepare_directories_changes_only_fixed_directory_and_database_objects(self):
        pool_root = self.pool_base / "pool-a"
        pool_root.mkdir(mode=0o755)
        cartridge = pool_root / "library-a" / "cartridge-a"
        cartridge.mkdir(parents=True)
        payload = cartridge / "segment.bin"
        payload.write_bytes(b"media-data")
        payload_inode = (payload.stat().st_dev, payload.stat().st_ino)
        database = self.data_dir / "holo.db"
        database.write_bytes(b"sqlite-fixture")
        database_inode = (database.stat().st_dev, database.stat().st_ino)
        chowned_inodes = []

        def capture_fchown(fd, uid, gid):
            info = os.fstat(fd)
            chowned_inodes.append((info.st_dev, info.st_ino))

        with patch.object(helper_module.os, "fchown", side_effect=capture_fchown):
            self.helper.execute(["prepare-directories"])

        self.assertEqual(payload.read_bytes(), b"media-data")
        self.assertEqual(database.read_bytes(), b"sqlite-fixture")
        self.assertNotIn(payload_inode, chowned_inodes)
        self.assertIn(database_inode, chowned_inodes)
        self.assertEqual(stat.S_IMODE(self.data_dir.stat().st_mode), 0o1770)
        self.assertEqual(stat.S_IMODE(self.pool_base.stat().st_mode), 0o755)
        self.assertEqual(stat.S_IMODE(pool_root.stat().st_mode), 0o755)
        self.assertEqual(stat.S_IMODE(database.stat().st_mode), 0o600)

    def test_prepare_directories_rejects_hardlinked_database_without_rewriting_it(self):
        database = self.data_dir / "holo.db"
        database.write_bytes(b"sqlite-fixture")
        outside_link = self.root / "outside.db"
        os.link(database, outside_link)
        before = database.read_bytes()

        with patch.object(helper_module.os, "fchown") as fchown:
            with self.assertRaises(HelperError):
                self.helper.execute(["prepare-directories"])

        self.assertEqual(database.read_bytes(), before)
        self.assertEqual(outside_link.read_bytes(), before)
        fchown.assert_called_once()

    def test_prepare_directories_preserves_service_access_to_mounted_xfs_root(self):
        pool_root = self.pool_base / "pool-mounted"
        pool_root.mkdir(mode=0o755)
        pool_inode = (pool_root.stat().st_dev, pool_root.stat().st_ino)
        chowned = []

        def capture_fchown(fd, uid, gid):
            info = os.fstat(fd)
            chowned.append(((info.st_dev, info.st_ino), uid, gid))

        def mounted_xfs(fd):
            return (self.helper._device_number(os.fstat(fd).st_dev), "xfs")

        with patch.object(self.helper, "_pool_mount_state", side_effect=mounted_xfs):
            with patch.object(helper_module.os, "fchown", side_effect=capture_fchown):
                self.helper.execute(["prepare-directories"])

        self.assertIn((pool_inode, self.helper.service_uid, self.helper.service_gid), chowned)
        self.assertEqual(stat.S_IMODE(pool_root.stat().st_mode), 0o750)

    def test_prepare_directories_stops_on_unexpected_pool_mount_type(self):
        pool_root = self.pool_base / "pool-mounted"
        pool_root.mkdir(mode=0o755)

        with patch.object(self.helper, "_pool_mount_state", return_value=("8:16", "ext4")):
            with patch.object(helper_module.os, "fchown"):
                with self.assertRaisesRegex(HelperError, "unsupported_pool_mount"):
                    self.helper.execute(["prepare-directories"])

    def test_refuses_non_block_and_system_in_use_devices(self):
        with self.assertRaises(HelperError):
            self.helper.execute(["mkfs.xfs", "-f", "/dev/null"])
        self.assertEqual(self.runner.calls, [])

    def test_rejects_device_major_minor_mismatch_after_open(self):
        fixture = self.root / "opened-device-fixture"
        fixture.write_bytes(b"")
        fd = os.open(fixture, os.O_RDONLY)
        self.addCleanup(os.close, fd)
        self.runner.lsblk_payload = {"blockdevices": [{"maj:min": "8:16", "type": "disk", "children": []}]}

        with self.assertRaisesRegex(HelperError, "device_identity_mismatch"):
            self.helper._device_tree(fd)
        self.assertEqual(len(self.runner.calls), 1)

    def test_refuses_a_mounted_device_even_when_lsblk_claims_unused(self):
        mountinfo = "1 0 8:16 / /mnt/test rw - xfs /dev/sdb rw\n"
        opened = []

        def fake_open(path, *args, **kwargs):
            opened.append(path)
            if path == "/proc/self/mountinfo":
                return io.StringIO(mountinfo)
            if path == "/proc/swaps":
                return io.StringIO("Filename Type Size Used Priority\n")
            raise AssertionError(path)

        with patch.object(builtins, "open", side_effect=fake_open):
            self.assertTrue(self.helper._device_is_mounted_or_swap("8:16"))
        self.assertEqual(opened, ["/proc/self/mountinfo"])

    def test_refuses_a_swap_device(self):
        mountinfo = "1 0 8:32 / / rw - ext4 /dev/root rw\n"
        swaps = "Filename Type Size Used Priority\n/dev/test-swap partition 1024 0 -2\n"
        device_info = SimpleNamespace(st_mode=stat.S_IFBLK, st_rdev=os.makedev(8, 16))

        def fake_open(path, *args, **kwargs):
            return io.StringIO(mountinfo if path == "/proc/self/mountinfo" else swaps)

        with patch.object(builtins, "open", side_effect=fake_open):
            with patch.object(helper_module.os, "stat", return_value=device_info):
                self.assertTrue(self.helper._device_is_mounted_or_swap("8:16"))

    def test_rejects_extra_arguments_before_any_command_runs(self):
        with self.assertRaises(HelperError):
            self.helper.execute(["mkdir", "-p", str(self.pool_base / "pool-a"), "/tmp/also"])
        self.assertEqual(self.runner.calls, [])


if __name__ == "__main__":
    unittest.main()
