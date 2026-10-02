#!/usr/bin/env python3
"""Restricted JSON helper for Holo-owned LIO loopback tape mappings."""

import fcntl
import errno
import hashlib
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import time
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple


PROTOCOL_VERSION = 1
MAX_INPUT = 64 * 1024
LOCK_PATH = "/run/holo/local-loopback-helper/lock"
OWNER_DIR = "/var/lib/holo/local-loopback"
CONFIGFS_ROOT = "/sys/kernel/config/target/loopback"
SCSI_HOST_ROOT = "/sys/class/scsi_host"
SCSI_GENERIC_ROOT = "/sys/class/scsi_generic"
SCSI_DEVICE_ROOT = "/sys/bus/scsi/devices"
SCSI_TAPE_ROOT = "/sys/class/st"
MANAGEMENT_ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._: -]{0,127}$")
NAA_RE = re.compile(r"^naa\.5[0-9a-f]{15}$")
BACKEND_RE = re.compile(r"^holo_[A-Za-z0-9_.-]{1,127}$")
HCTL_RE = re.compile(r"^[0-9]+:[0-9]+:[0-9]+:[0-9]+$")


class InvalidRequest(ValueError):
    pass


class HelperFailure(RuntimeError):
    def __init__(self, reason: str):
        super().__init__(reason)
        self.reason = reason


def _object_no_duplicates(pairs: List[Tuple[str, Any]]) -> Dict[str, Any]:
    result = {}
    for key, value in pairs:
        if key in result:
            raise InvalidRequest("invalid_json")
        result[key] = value
    return result


def _keys(value: Any, required: Tuple[str, ...], optional: Tuple[str, ...] = ()) -> None:
    if not isinstance(value, dict) or not set(required).issubset(value):
        raise InvalidRequest("invalid_schema")
    if set(value) - set(required) - set(optional):
        raise InvalidRequest("invalid_schema")


def _management_id(value: Any) -> bool:
    return isinstance(value, str) and MANAGEMENT_ID_RE.fullmatch(value) is not None and ".." not in value


def validate_mapping(mapping: Any, operation: str) -> Dict[str, Any]:
    _keys(mapping, ("libraryId", "targetNaa", "nexusNaa", "tpgTag", "devices"))
    library_id = mapping["libraryId"]
    target_naa = mapping["targetNaa"]
    nexus_naa = mapping["nexusNaa"]
    if not _management_id(library_id):
        raise InvalidRequest("invalid_mapping")
    if not isinstance(target_naa, str) or not NAA_RE.fullmatch(target_naa):
        raise InvalidRequest("invalid_mapping")
    if not isinstance(nexus_naa, str) or not NAA_RE.fullmatch(nexus_naa) or nexus_naa == target_naa:
        raise InvalidRequest("invalid_mapping")
    if type(mapping["tpgTag"]) is not int or mapping["tpgTag"] != 1:
        raise InvalidRequest("invalid_mapping")
    devices = mapping["devices"]
    if not isinstance(devices, list) or len(devices) > 5:
        raise InvalidRequest("invalid_mapping")
    normalized = []
    keys = set()
    luns = set()
    identities = set()
    backends = set()
    changer_count = 0
    for device in devices:
        _keys(device, ("deviceKey", "kind", "lun", "identityRef", "backendRef", "state"), ("driveId",))
        kind = device["kind"]
        device_key = device["deviceKey"]
        drive_id = device.get("driveId", "")
        lun = device["lun"]
        identity_ref = device["identityRef"]
        backend_ref = device["backendRef"]
        state = device["state"]
        if type(lun) is not int or not _management_id(identity_ref):
            raise InvalidRequest("invalid_mapping")
        if not isinstance(backend_ref, str) or not BACKEND_RE.fullmatch(backend_ref) or ".." in backend_ref:
            raise InvalidRequest("invalid_mapping")
        if state not in ("active", "cleanup_pending"):
            raise InvalidRequest("invalid_mapping")
        if kind == "changer":
            changer_count += 1
            if device_key != "changer:" + library_id or drive_id not in (None, "") or lun != 0:
                raise InvalidRequest("invalid_mapping")
            drive_id = ""
        elif kind == "drive":
            if not _management_id(drive_id) or device_key != "drive:" + drive_id or not 1 <= lun <= 65535:
                raise InvalidRequest("invalid_mapping")
        else:
            raise InvalidRequest("invalid_mapping")
        if device_key in keys or lun in luns or identity_ref in identities or backend_ref in backends:
            raise InvalidRequest("invalid_mapping")
        keys.add(device_key)
        luns.add(lun)
        identities.add(identity_ref)
        backends.add(backend_ref)
        normalized.append({
            "deviceKey": device_key,
            "kind": kind,
            "driveId": drive_id,
            "lun": lun,
            "identityRef": identity_ref,
            "backendRef": backend_ref,
            "state": state,
        })
    if devices and changer_count != 1:
        raise InvalidRequest("invalid_mapping")
    if operation == "ensure" and any(device["state"] != "active" for device in normalized):
        raise InvalidRequest("invalid_mapping")
    result = dict(mapping)
    result["devices"] = sorted(normalized, key=lambda device: device["lun"])
    return result


class LocalLoopbackHelper:
    def __init__(self, backend: Any, lock_path: str = LOCK_PATH, lock_timeout: float = 10.0):
        self.backend = backend
        self.lock_path = lock_path
        self.lock_timeout = lock_timeout

    def handle(self, payload: bytes) -> Dict[str, Any]:
        try:
            request = self._parse(payload)
            operation = request["operation"]
            if operation == "probe":
                return {"version": PROTOCOL_VERSION, "ok": True, "result": self.backend.probe()}
            if operation == "list":
                return {"version": PROTOCOL_VERSION, "ok": True, "result": self.backend.list_owned()}
            mapping = validate_mapping(request["mapping"], operation)
            with self._exclusive_lock():
                return self._mutate(operation, mapping)
        except InvalidRequest as exc:
            return self._error(str(exc))
        except HelperFailure as exc:
            return self._error(exc.reason)
        except TimeoutError:
            return self._error("operation_timeout")
        except Exception:
            return self._error("operation_failed")

    def _parse(self, payload: bytes) -> Dict[str, Any]:
        if not isinstance(payload, (bytes, bytearray)) or len(payload) > MAX_INPUT:
            raise InvalidRequest("invalid_size")
        try:
            request = json.loads(bytes(payload).decode("utf-8"), object_pairs_hook=_object_no_duplicates)
        except (UnicodeDecodeError, json.JSONDecodeError):
            raise InvalidRequest("invalid_json")
        if not isinstance(request, dict) or type(request.get("version")) is not int or request["version"] != PROTOCOL_VERSION:
            raise InvalidRequest("invalid_envelope")
        operation = request.get("operation")
        if operation in ("probe", "list"):
            _keys(request, ("version", "operation"))
        elif operation in ("ensure", "remove"):
            _keys(request, ("version", "operation", "mapping"))
        else:
            raise InvalidRequest("unsupported_operation")
        return request

    def _exclusive_lock(self):
        return _ExclusiveLock(self.lock_path, self.lock_timeout)

    def _mutate(self, operation: str, mapping: Dict[str, Any]) -> Dict[str, Any]:
        ownership = self.backend.inspect(mapping)
        if ownership == "conflict":
            raise HelperFailure("mapping_conflict")
        if ownership not in ("absent", "owned"):
            raise HelperFailure("operation_failed")
        if operation == "ensure":
            return self._ensure(mapping)
        return self._remove(mapping)

    def _ensure(self, mapping: Dict[str, Any]) -> Dict[str, Any]:
        components = ["target", "tpg", "nexus"]
        created = []
        try:
            for component in components:
                if self.backend.ensure_component(mapping, component):
                    created.append(component)
        except Exception:
            cleanup_failed = False
            for component in reversed(created):
                try:
                    self.backend.remove_component(mapping, component)
                except Exception:
                    cleanup_failed = True
            if cleanup_failed:
                raise HelperFailure("cleanup_failed")
            raise HelperFailure("operation_failed")
        failures = {}
        for device in mapping["devices"]:
            try:
                self.backend.ensure_component(mapping, "lun:" + str(device["lun"]))
            except Exception as exc:
                failures[device["deviceKey"]] = self._safe_reason(exc)
        return {
            "version": PROTOCOL_VERSION,
            "ok": True,
            "result": self._apply_device_failures(self.backend.observe(mapping), failures),
        }

    def _remove(self, mapping: Dict[str, Any]) -> Dict[str, Any]:
        pending = [device for device in mapping["devices"] if device["state"] == "cleanup_pending"]
        failures = {}
        for device in pending:
            try:
                self.backend.remove_component(mapping, "lun:" + str(device["lun"]))
            except Exception as exc:
                failures[device["deviceKey"]] = self._safe_reason(exc)
        active = [device for device in mapping["devices"] if device["state"] == "active"]
        if not active and not failures:
            for component in ("nexus", "tpg", "target"):
                self.backend.remove_component(mapping, component)
        return {
            "version": PROTOCOL_VERSION,
            "ok": True,
            "result": self._apply_device_failures(self.backend.observe(mapping), failures, removing=True),
        }

    @staticmethod
    def _safe_reason(exc: Exception) -> str:
        if isinstance(exc, HelperFailure) and exc.reason in (
            "mapping_conflict", "backend_not_ready", "device_busy", "operation_timeout", "cleanup_failed"
        ):
            return exc.reason
        if isinstance(exc, subprocess.TimeoutExpired) or isinstance(exc, TimeoutError):
            return "operation_timeout"
        if isinstance(exc, OSError) and exc.errno == errno.EBUSY:
            return "device_busy"
        return "cleanup_failed" if isinstance(exc, OSError) else "backend_not_ready"

    @staticmethod
    def _apply_device_failures(observed: Any, failures: Dict[str, str], removing: bool = False) -> Any:
        if not failures or not isinstance(observed, list):
            return observed
        result = []
        seen = set()
        for item in observed:
            if not isinstance(item, dict):
                continue
            device_key = item.get("deviceKey")
            if device_key in failures:
                item = dict(item)
                item["state"] = "residual" if removing else "failed"
                item["reasonCode"] = failures[device_key]
            result.append(item)
            seen.add(device_key)
        for device_key, reason in failures.items():
            if device_key not in seen:
                result.append({
                    "deviceKey": device_key,
                    "state": "residual" if removing else "failed",
                    "observedPaths": [],
                    "reasonCode": reason,
                })
        return result

    @staticmethod
    def _error(reason: str) -> Dict[str, Any]:
        return {"version": PROTOCOL_VERSION, "ok": False, "reason": reason}


class _ExclusiveLock:
    def __init__(self, path: str, timeout: float):
        self.path = path
        self.timeout = timeout
        self.fd = None

    def __enter__(self):
        parent = os.path.dirname(self.path)
        if parent:
            os.makedirs(parent, mode=0o700, exist_ok=True)
            parent_stat = os.stat(parent)
            if parent_stat.st_uid != os.geteuid() or parent_stat.st_mode & 0o022:
                raise HelperFailure("operation_failed")
        flags = os.O_CREAT | os.O_RDWR
        if hasattr(os, "O_NOFOLLOW"):
            flags |= os.O_NOFOLLOW
        self.fd = os.open(self.path, flags, 0o600)
        lock_stat = os.fstat(self.fd)
        if not stat.S_ISREG(lock_stat.st_mode) or lock_stat.st_uid != os.geteuid() or lock_stat.st_mode & 0o077:
            os.close(self.fd)
            self.fd = None
            raise HelperFailure("operation_failed")
        deadline = time.monotonic() + self.timeout
        while True:
            try:
                fcntl.flock(self.fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                return self
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    os.close(self.fd)
                    self.fd = None
                    raise TimeoutError("operation lock deadline exceeded")
                time.sleep(min(0.025, max(0, deadline - time.monotonic())))

    def __exit__(self, _kind, _value, _traceback):
        if self.fd is not None:
            fcntl.flock(self.fd, fcntl.LOCK_UN)
            os.close(self.fd)
            self.fd = None


class RTSlibLoopbackBackend:
    def __init__(self):
        from rtslib import FabricModule, LUN, RTSRoot, TPG, Target

        self.FabricModule = FabricModule
        self.LUN = LUN
        self.RTSRoot = RTSRoot
        self.TPG = TPG
        self.Target = Target
        self.configfs_root = Path(CONFIGFS_ROOT)
        self.owner_dir = Path(OWNER_DIR)
        self._storage_objects = None

    def probe(self) -> Dict[str, Any]:
        if os.geteuid() != 0 or not self.configfs_root.is_dir() or shutil.which("sg_inq") is None:
            return {"available": False, "reasonCode": "loopback_unavailable"}
        try:
            self.FabricModule("loopback")._check_self()
        except Exception:
            return {"available": False, "reasonCode": "loopback_unavailable"}
        return {"available": True}

    def list_owned(self) -> List[Dict[str, Any]]:
        result = []
        owned_targets = set()
        if not self.owner_dir.is_dir():
            marker_paths = []
        else:
            marker_paths = sorted(self.owner_dir.glob("*.json"))
        for path in marker_paths:
            owner = self._read_owner_file(path)
            if not isinstance(owner, dict):
                raise HelperFailure("mapping_conflict")
            if (
                type(owner.get("version")) is not int
                or owner.get("version") != PROTOCOL_VERSION
                or not _management_id(owner.get("libraryId"))
                or not isinstance(owner.get("targetNaa"), str)
                or not NAA_RE.fullmatch(owner["targetNaa"])
                or not isinstance(owner.get("nexusNaa"), str)
                or not NAA_RE.fullmatch(owner["nexusNaa"])
                or type(owner.get("tpgTag")) is not int
                or owner.get("tpgTag") != 1
            ):
                raise HelperFailure("mapping_conflict")
            devices = owner.get("devices")
            if not isinstance(devices, list):
                raise HelperFailure("mapping_conflict")
            mapping = {
                "libraryId": owner["libraryId"],
                "targetNaa": owner["targetNaa"],
                "nexusNaa": owner["nexusNaa"],
                "tpgTag": owner["tpgTag"],
                "devices": [
                    {**device, "state": "cleanup_pending"}
                    for device in devices
                    if isinstance(device, dict)
                ],
            }
            try:
                mapping = validate_mapping(mapping, "remove")
            except InvalidRequest:
                raise HelperFailure("mapping_conflict")
            if len(mapping["devices"]) != len(devices):
                raise HelperFailure("mapping_conflict")
            target_naa = owner["targetNaa"]
            owned_targets.add(target_naa)
            target_path = self.configfs_root / target_naa
            result.append({
                "libraryId": owner["libraryId"],
                "targetNaa": target_naa,
                "nexusNaa": owner["nexusNaa"],
                "tpgTag": owner["tpgTag"],
                "devices": mapping["devices"],
                "present": target_path.is_dir(),
            })
        for target_path in self.configfs_root.glob("naa.*"):
            if not target_path.is_dir() or target_path.name in owned_targets:
                continue
            for lun_path in target_path.glob("tpgt_*/lun/lun_*"):
                for link in lun_path.iterdir():
                    if not link.is_symlink():
                        continue
                    try:
                        storage_path = Path(os.path.realpath(link))
                    except OSError:
                        continue
                    if storage_path.parent.name.startswith("user") and storage_path.name.startswith("holo_"):
                        raise HelperFailure("mapping_conflict")
        return result

    def inspect(self, mapping: Dict[str, Any]) -> str:
        target_path = self.configfs_root / mapping["targetNaa"]
        owner_path = self._owner_path(mapping["libraryId"])
        owner = self._read_owner_file(owner_path)
        expected_owner = self._owner_record(mapping)
        if owner is None:
            return "conflict" if target_path.exists() else "absent"
        if owner != expected_owner:
            return "conflict"
        if not target_path.exists():
            return "owned"
        target = self._target(mapping)
        if target is None:
            return "conflict"
        tpg_paths = sorted(path for path in target_path.glob("tpgt_*") if path.is_dir())
        if any(path.name != "tpgt_1" for path in tpg_paths) or len(tpg_paths) > 1:
            return "conflict"
        if not tpg_paths:
            return "owned"
        try:
            nexus = (tpg_paths[0] / "nexus").read_text(encoding="ascii").strip()
        except OSError:
            nexus = ""
        if nexus and nexus != mapping["nexusNaa"]:
            return "conflict"
        expected = {device["lun"]: "user:" + device["backendRef"] for device in mapping["devices"]}
        lun_root = tpg_paths[0] / "lun"
        for lun_path in lun_root.glob("lun_*"):
            try:
                lun_id = int(lun_path.name.split("_", 1)[1])
                storage = lun_path / "holo-backstore"
                if not storage.is_symlink():
                    links = [item for item in lun_path.iterdir() if item.is_symlink()]
                    if len(links) != 1:
                        return "conflict"
                    storage = links[0]
                backend = "user:" + storage.resolve().name
            except (OSError, ValueError):
                return "conflict"
            if expected.get(lun_id) != backend:
                return "conflict"
            self._storage_object(backend.split(":", 1)[1])
        return "owned"

    def ensure_component(self, mapping: Dict[str, Any], component: str) -> bool:
        if component == "target":
            path = self.configfs_root / mapping["targetNaa"]
            if path.exists():
                if self.inspect(mapping) != "owned":
                    raise HelperFailure("mapping_conflict")
                return False
            owner_created = self._write_owner(mapping)
            try:
                self.Target(self.FabricModule("loopback"), mapping["targetNaa"], mode="create")
            except Exception:
                if owner_created and not path.exists():
                    self._clear_owner(mapping)
                raise
            return True
        target = self._target(mapping)
        if target is None:
            raise HelperFailure("mapping_conflict")
        tpg_path = Path(target.path) / "tpgt_1"
        if component == "tpg":
            if tpg_path.exists():
                return False
            os.mkdir(tpg_path, 0o755)
            return True
        if component == "nexus":
            if not tpg_path.is_dir():
                raise HelperFailure("mapping_conflict")
            nexus_path = tpg_path / "nexus"
            try:
                existing = nexus_path.read_text(encoding="ascii").strip()
            except OSError:
                existing = ""
            if existing == mapping["nexusNaa"]:
                return False
            if existing:
                raise HelperFailure("mapping_conflict")
            with nexus_path.open("w", encoding="ascii") as stream:
                stream.write(mapping["nexusNaa"])
            tpg = self.TPG(target, tag=1, mode="lookup")
            if tpg.nexus != mapping["nexusNaa"]:
                raise HelperFailure("mapping_conflict")
            return True
        if component.startswith("lun:"):
            lun_id = int(component.split(":", 1)[1])
            device = next((item for item in mapping["devices"] if item["lun"] == lun_id), None)
            if device is None:
                raise HelperFailure("mapping_conflict")
            tpg = self.TPG(target, tag=1, mode="lookup")
            lun_path = Path(tpg.path) / "lun" / ("lun_" + str(lun_id))
            if lun_path.exists():
                lun = self._lookup_lun(tpg, lun_id)
                storage = lun.storage_object
                if storage.plugin != "user" or storage.name != device["backendRef"]:
                    raise HelperFailure("mapping_conflict")
                return False
            self.LUN(tpg, lun=lun_id, storage_object=self._storage_object(device["backendRef"]))
            return True
        raise HelperFailure("unsupported_operation")

    def remove_component(self, mapping: Dict[str, Any], component: str) -> None:
        target = self._target(mapping)
        if component == "target":
            if target is None:
                self._clear_owner(mapping)
                return
            target_path = self.configfs_root / mapping["targetNaa"]
            if any(path.is_dir() for path in target_path.glob("tpgt_*")):
                raise HelperFailure("cleanup_failed")
            target.delete()
            if (self.configfs_root / mapping["targetNaa"]).exists():
                raise HelperFailure("cleanup_failed")
            self._clear_owner(mapping)
            return
        if target is None:
            return
        tpg_path = Path(target.path) / "tpgt_1"
        if not tpg_path.is_dir():
            return
        if component == "tpg":
            nexus_path = tpg_path / "nexus"
            try:
                nexus = nexus_path.read_text(encoding="ascii").strip()
            except OSError:
                nexus = ""
            if nexus or any((tpg_path / "lun").glob("lun_*")):
                raise HelperFailure("cleanup_failed")
            os.rmdir(tpg_path)
            if tpg_path.exists():
                raise HelperFailure("cleanup_failed")
            return
        if component.startswith("lun:"):
            lun_id = int(component.split(":", 1)[1])
            device = next((item for item in mapping["devices"] if item["lun"] == lun_id), None)
            if device is None:
                raise HelperFailure("mapping_conflict")
            lun_path = tpg_path / "lun" / ("lun_" + str(lun_id))
            if lun_path.exists():
                links = [path for path in lun_path.iterdir() if path.is_symlink()]
                if len(links) != 1:
                    raise HelperFailure("mapping_conflict")
                storage_path = Path(os.path.realpath(links[0]))
                if storage_path.name != device["backendRef"] or "user" not in storage_path.parent.name:
                    raise HelperFailure("mapping_conflict")
                try:
                    nexus = (tpg_path / "nexus").read_text(encoding="ascii").strip()
                except OSError:
                    nexus = ""
                if nexus:
                    tpg = self.TPG(target, tag=1, mode="lookup")
                    lun = self._lookup_lun(tpg, lun_id)
                    storage = lun.storage_object
                    if storage.plugin != "user" or storage.name != device["backendRef"]:
                        raise HelperFailure("mapping_conflict")
                    lun.delete()
                else:
                    links[0].unlink()
                    os.rmdir(lun_path)
            return
        if component == "nexus":
            nexus_path = tpg_path / "nexus"
            try:
                nexus = nexus_path.read_text(encoding="ascii").strip()
            except OSError:
                nexus = ""
            if nexus:
                if nexus != mapping["nexusNaa"]:
                    raise HelperFailure("mapping_conflict")
                with nexus_path.open("w", encoding="ascii") as stream:
                    stream.write("NULL")
                try:
                    remaining = nexus_path.read_text(encoding="ascii").strip()
                except OSError:
                    remaining = ""
                if remaining:
                    raise HelperFailure("cleanup_failed")
            return
        tpg = self.TPG(target, tag=1, mode="lookup")
        raise HelperFailure("unsupported_operation")

    def observe(self, mapping: Dict[str, Any]) -> List[Dict[str, Any]]:
        found = self._enumerated_scsi_devices()
        observed = []
        for device in mapping["devices"]:
            identity_matches = [item for item in found if item["serial"] == device["identityRef"]]
            matches = [item for item in identity_matches if item["lun"] == device["lun"]]
            if len(matches) > 1:
                observed.append({"deviceKey": device["deviceKey"], "state": "failed", "observedPaths": [], "reasonCode": "identity_conflict"})
                continue
            if not matches:
                conflict = identity_matches or any(item["lun"] == device["lun"] for item in found)
                observed.append({
                    "deviceKey": device["deviceKey"],
                    "state": "failed" if conflict else "pending",
                    "observedPaths": [],
                    "reasonCode": "identity_conflict" if conflict else "device_not_enumerated",
                })
                continue
            item = matches[0]
            expected_type = 8 if device["kind"] == "changer" else 1
            if item["deviceType"] != expected_type:
                observed.append({"deviceKey": device["deviceKey"], "state": "failed", "observedPaths": [], "reasonCode": "identity_conflict"})
                continue
            observed.append({
                "deviceKey": device["deviceKey"],
                "state": "connected",
                "observedPaths": item["paths"],
                "reasonCode": "",
            })
        return observed

    def _enumerated_scsi_devices(self) -> List[Dict[str, Any]]:
        hosts = {}
        for host_path in Path(SCSI_HOST_ROOT).glob("host*"):
            try:
                proc_name = (host_path / "proc_name").read_text(encoding="utf-8").strip()
            except OSError:
                continue
            if proc_name in ("tcm_loopback", "tcm_loop"):
                hosts[host_path.name[4:]] = True
        results = []
        for sg_path in Path(SCSI_GENERIC_ROOT).glob("sg*"):
            try:
                hctl = Path(os.path.realpath(sg_path / "device")).name
                if not HCTL_RE.fullmatch(hctl):
                    continue
                host_id, _channel, _target, lun_text = hctl.split(":")
                if host_id not in hosts:
                    continue
                scsi_device = Path(SCSI_DEVICE_ROOT) / hctl
                device_type = int((scsi_device / "type").read_text(encoding="ascii").strip())
                result = subprocess.run(
                    ["sg_inq", "--page=0x80", "/dev/" + sg_path.name],
                    text=True,
                    capture_output=True,
                    timeout=3,
                    check=False,
                )
                if result.returncode != 0:
                    continue
                match = re.search(r"Unit serial number:\s*([^\r\n]+)", result.stdout, re.IGNORECASE)
                if not match:
                    continue
                paths = ["/dev/" + sg_path.name]
                for st_path in Path(SCSI_TAPE_ROOT).glob("st*"):
                    try:
                        if Path(os.path.realpath(st_path / "device")).name == hctl:
                            paths.append("/dev/" + st_path.name)
                    except OSError:
                        continue
                results.append({
                    "lun": int(lun_text),
                    "serial": match.group(1).strip(),
                    "deviceType": device_type,
                    "paths": paths,
                })
            except (OSError, ValueError, subprocess.TimeoutExpired):
                continue
        return results

    def _target(self, mapping: Dict[str, Any]):
        path = self.configfs_root / mapping["targetNaa"]
        if not path.is_dir():
            return None
        return self.Target(self.FabricModule("loopback"), mapping["targetNaa"], mode="lookup")

    def _storage_object(self, name: str):
        if self._storage_objects is None:
            self._storage_objects = {
                storage.name: storage
                for storage in self.RTSRoot().storage_objects
                if storage.plugin == "user" and storage.name.startswith("holo_")
            }
        storage = self._storage_objects.get(name)
        if storage is None or storage.plugin != "user":
            raise HelperFailure("mapping_conflict")
        return storage

    def _lookup_lun(self, tpg: Any, lun_id: int):
        # rtslib uses storage_object=None to select lookup mode. Unlike TPG,
        # LUN does not accept a mode keyword.
        return self.LUN(tpg, lun=lun_id)

    def _owner_path(self, library_id: str) -> Path:
        suffix = hashlib.sha256(library_id.encode("utf-8")).hexdigest()
        return self.owner_dir / (suffix + ".json")

    @staticmethod
    def _owner_record(mapping: Dict[str, Any]) -> Dict[str, Any]:
        return {
            "version": PROTOCOL_VERSION,
            "libraryId": mapping["libraryId"],
            "targetNaa": mapping["targetNaa"],
            "nexusNaa": mapping["nexusNaa"],
            "tpgTag": mapping["tpgTag"],
            "devices": [
                {
                    "deviceKey": device["deviceKey"],
                    "kind": device["kind"],
                    "driveId": device["driveId"],
                    "lun": device["lun"],
                    "identityRef": device["identityRef"],
                    "backendRef": device["backendRef"],
                }
                for device in mapping["devices"]
            ],
        }

    def _read_owner_file(self, path: Path) -> Optional[Dict[str, Any]]:
        fd = None
        try:
            flags = os.O_RDONLY
            if hasattr(os, "O_NOFOLLOW"):
                flags |= os.O_NOFOLLOW
            fd = os.open(str(path), flags)
            metadata = os.fstat(fd)
            if metadata.st_uid != 0 or metadata.st_mode & 0o077:
                return {"invalid": True}
            with os.fdopen(fd, "r", encoding="utf-8") as stream:
                fd = None
                value = json.load(stream, object_pairs_hook=_object_no_duplicates)
            return value if isinstance(value, dict) else {"invalid": True}
        except FileNotFoundError:
            return None
        except (OSError, json.JSONDecodeError, InvalidRequest):
            return {"invalid": True}
        finally:
            if fd is not None:
                os.close(fd)

    def _write_owner(self, mapping: Dict[str, Any]) -> bool:
        try:
            self.owner_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        except OSError:
            raise HelperFailure("operation_failed")
        try:
            dir_metadata = self.owner_dir.lstat()
        except OSError:
            raise HelperFailure("operation_failed")
        if self.owner_dir.is_symlink() or dir_metadata.st_uid != 0 or dir_metadata.st_mode & 0o022:
            raise HelperFailure("mapping_conflict")
        owner_path = self._owner_path(mapping["libraryId"])
        expected = self._owner_record(mapping)
        existing = self._read_owner_file(owner_path)
        if existing is not None:
            if existing != expected:
                raise HelperFailure("mapping_conflict")
            return False
        payload = json.dumps(expected, sort_keys=True, separators=(",", ":")).encode("utf-8")
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
        if hasattr(os, "O_NOFOLLOW"):
            flags |= os.O_NOFOLLOW
        try:
            fd = os.open(str(owner_path), flags, 0o600)
        except FileExistsError:
            if self._read_owner_file(owner_path) == expected:
                return False
            raise HelperFailure("mapping_conflict")
        with os.fdopen(fd, "wb") as stream:
            stream.write(payload)
            stream.flush()
            os.fsync(stream.fileno())
        return True

    def _clear_owner(self, mapping: Dict[str, Any]) -> None:
        owner_path = self._owner_path(mapping["libraryId"])
        existing = self._read_owner_file(owner_path)
        if existing is None:
            return
        if existing != self._owner_record(mapping):
            raise HelperFailure("mapping_conflict")
        owner_path.unlink()


def main() -> int:
    try:
        backend = RTSlibLoopbackBackend()
        helper = LocalLoopbackHelper(backend)
        response = helper.handle(sys.stdin.buffer.read(MAX_INPUT + 1))
    except Exception:
        response = LocalLoopbackHelper._error("loopback_unavailable")
    sys.stdout.write(json.dumps(response, sort_keys=True, separators=(",", ":")) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
