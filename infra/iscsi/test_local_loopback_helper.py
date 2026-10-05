import importlib.util
import json
import fcntl
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest.mock import patch


HELPER_PATH = Path(__file__).with_name("holo-local-loopback-helper.py")
spec = importlib.util.spec_from_file_location("holo_local_loopback_helper", HELPER_PATH)
helper_module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper_module)
LocalLoopbackHelper = helper_module.LocalLoopbackHelper


class FakeLoopbackBackend:
    def __init__(self):
        self.present = set()
        self.created = []
        self.removed = []
        self.conflict = False
        self.fail_component = None
        self.fail_remove_component = None
        self.delay = 0
        self.active = 0
        self.max_active = 0
        self.lock = threading.Lock()

    def inspect(self, _mapping):
        if self.conflict:
            return "conflict"
        if not self.present:
            return "absent"
        return "owned"

    def inspect_for_remove(self, mapping):
        return self.inspect(mapping)

    def probe(self):
        return {"available": True}

    def list_owned(self):
        return []

    def ensure_component(self, _mapping, component):
        with self.lock:
            self.active += 1
            self.max_active = max(self.max_active, self.active)
        try:
            if self.delay:
                time.sleep(self.delay)
            if component == self.fail_component:
                raise RuntimeError("injected backend failure with secret=do-not-return")
            if component in self.present:
                return False
            self.present.add(component)
            self.created.append(component)
            return True
        finally:
            with self.lock:
                self.active -= 1

    def remove_component(self, _mapping, component):
        if component == self.fail_remove_component:
            raise RuntimeError("injected remove failure")
        if component in self.present:
            self.removed.append(component)
            self.present.discard(component)

    def observe(self, _mapping):
        return [
            {
                "deviceKey": device["deviceKey"],
                "state": "connected" if "lun:" + str(device["lun"]) in self.present else "pending",
                "observedPaths": [],
            }
            for device in _mapping["devices"]
        ]


def valid_request(operation="ensure"):
    return {
        "version": 1,
        "operation": operation,
        "mapping": {
            "libraryId": "library-a",
            "targetNaa": "naa.50014056b18af0f5",
            "nexusNaa": "naa.5001405db2f4505b",
            "tpgTag": 1,
            "devices": [
                {
                    "deviceKey": "changer:library-a",
                    "kind": "changer",
                    "driveId": "",
                    "lun": 0,
                    "identityRef": "changer-identity-a",
                    "backendRef": "holo_backstore_changer",
                    "state": "cleanup_pending" if operation == "remove" else "active",
                },
                {
                    "deviceKey": "drive:drive-a",
                    "kind": "drive",
                    "driveId": "drive-a",
                    "lun": 5,
                    "identityRef": "drive-identity-a",
                    "backendRef": "holo_backstore_drive_a",
                    "state": "cleanup_pending" if operation == "remove" else "active",
                },
            ],
        },
    }


class LocalLoopbackHelperTests(unittest.TestCase):
    def make_helper(self, backend, directory, lock_timeout=0.2):
        return LocalLoopbackHelper(backend, str(Path(directory) / "helper.lock"), lock_timeout=lock_timeout)

    def test_default_lock_is_isolated_from_service_owned_runtime_directory(self):
        self.assertEqual(helper_module.LOCK_PATH, "/run/holo/local-loopback-helper/lock")
        self.assertEqual(Path(helper_module.LOCK_PATH).parent.name, "local-loopback-helper")
        self.assertEqual(Path(helper_module.LOCK_PATH).parent.parent, Path("/run/holo"))

    def test_rtslib_lun_lookup_uses_storage_object_none_mode(self):
        backend = helper_module.RTSlibLoopbackBackend.__new__(helper_module.RTSlibLoopbackBackend)
        calls = []

        class FakeLUN:
            def __init__(self, parent_tpg, lun=None, storage_object=None, alias=None):
                calls.append((parent_tpg, lun, storage_object, alias))

        backend.LUN = FakeLUN
        result = backend._lookup_lun("tpg", 4)

        self.assertIsInstance(result, FakeLUN)
        self.assertEqual(calls, [("tpg", 4, None, None)])

    def test_probe_initializes_lazy_configfs_fabric_before_checking_path(self):
        with tempfile.TemporaryDirectory() as directory:
            backend = helper_module.RTSlibLoopbackBackend.__new__(helper_module.RTSlibLoopbackBackend)
            backend.configfs_root = Path(directory) / "target" / "loopback"
            calls = []

            class FakeFabricModule:
                def __init__(self, name):
                    calls.append(("init", name))

                def _check_self(self):
                    calls.append(("check",))
                    backend.configfs_root.mkdir(parents=True)

            backend.FabricModule = FakeFabricModule
            with patch.object(helper_module.os, "geteuid", return_value=0), patch.object(
                helper_module.shutil, "which", return_value="/usr/bin/sg_inq"
            ):
                result = backend.probe()

            self.assertEqual(result, {"available": True})
            self.assertTrue(backend.configfs_root.is_dir())
            self.assertEqual(calls, [("init", "loopback"), ("check",)])

    def test_remove_inspection_accepts_only_owned_empty_orphan_luns(self):
        with tempfile.TemporaryDirectory() as directory:
            backend = helper_module.RTSlibLoopbackBackend.__new__(helper_module.RTSlibLoopbackBackend)
            backend.configfs_root = Path(directory) / "target" / "loopback"
            backend.owner_dir = Path(directory) / "owners"
            mapping = helper_module.validate_mapping(valid_request("remove")["mapping"], "remove")
            target = backend.configfs_root / mapping["targetNaa"]
            tpg = target / "tpgt_1"
            lun_root = tpg / "lun"
            core = backend.configfs_root.parent / "core"
            lun_root.mkdir(parents=True)
            (tpg / "nexus").write_text(mapping["nexusNaa"], encoding="ascii")
            (lun_root / "lun_5").mkdir()
            for name in helper_module.CONFIGFS_LUN_ATTRIBUTES:
                (lun_root / "lun_5" / name).mkdir()
            storage = core / "user_1" / mapping["devices"][0]["backendRef"]
            storage.parent.mkdir(parents=True)
            (lun_root / "lun_0").mkdir()
            (lun_root / "lun_0" / "holo-backstore").symlink_to(storage)
            for name in helper_module.CONFIGFS_LUN_ATTRIBUTES:
                (lun_root / "lun_0" / name).mkdir()
            backend.owner_dir.mkdir()
            owner = backend._owner_record(mapping)
            with patch.object(backend, "_read_owner_file", return_value=owner):
                self.assertEqual(backend.inspect_for_remove(mapping), "owned")

                unexpected_lun = lun_root / "lun_99"
                unexpected_lun.mkdir()
                self.assertEqual(backend.inspect_for_remove(mapping), "conflict")

    def test_remove_component_deletes_only_an_empty_owned_lun_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            backend = helper_module.RTSlibLoopbackBackend.__new__(helper_module.RTSlibLoopbackBackend)
            backend.configfs_root = Path(directory) / "target" / "loopback"
            mapping = helper_module.validate_mapping(valid_request("remove")["mapping"], "remove")
            target = backend.configfs_root / mapping["targetNaa"]
            empty_lun = target / "tpgt_1" / "lun" / "lun_5"
            empty_lun.mkdir(parents=True)

            with patch.object(backend, "_target", return_value=type("Target", (), {"path": str(target)})()):
                backend.remove_component(mapping, "lun:5")
            self.assertFalse(empty_lun.exists())

            nonempty_lun = target / "tpgt_1" / "lun" / "lun_5"
            nonempty_lun.mkdir()
            (nonempty_lun / "unexpected").touch()
            with patch.object(backend, "_target", return_value=type("Target", (), {"path": str(target)})()):
                with self.assertRaises(helper_module.HelperFailure) as raised:
                    backend.remove_component(mapping, "lun:5")
            self.assertEqual(raised.exception.reason, "mapping_conflict")
            self.assertTrue(nonempty_lun.exists())

    def test_remove_component_accepts_owned_lun_with_configfs_attributes(self):
        with tempfile.TemporaryDirectory() as directory:
            backend = helper_module.RTSlibLoopbackBackend.__new__(helper_module.RTSlibLoopbackBackend)
            backend.configfs_root = Path(directory) / "target" / "loopback"
            mapping = helper_module.validate_mapping(valid_request("remove")["mapping"], "remove")
            target = backend.configfs_root / mapping["targetNaa"]
            tpg_path = target / "tpgt_1"
            lun_path = tpg_path / "lun" / "lun_0"
            lun_path.mkdir(parents=True)
            (tpg_path / "nexus").write_text(mapping["nexusNaa"], encoding="ascii")
            storage = Path(directory) / "core" / "user_1" / mapping["devices"][0]["backendRef"]
            storage.parent.mkdir(parents=True)
            (lun_path / "holo-backstore").symlink_to(storage)
            for name in helper_module.CONFIGFS_LUN_ATTRIBUTES:
                (lun_path / name).mkdir()

            class FakeStorage:
                plugin = "user"
                name = mapping["devices"][0]["backendRef"]

            class FakeLUN:
                storage_object = FakeStorage()

                def delete(self):
                    self.deleted = True

            fake_lun = FakeLUN()
            fake_target = type("Target", (), {"path": str(target)})()
            with patch.object(backend, "_target", return_value=fake_target), patch.object(
                backend, "TPG", create=True, return_value=object()
            ), patch.object(backend, "_lookup_lun", return_value=fake_lun):
                backend.remove_component(mapping, "lun:0")

            self.assertTrue(fake_lun.deleted)

    def test_json_version_operation_and_fields_are_allowlisted(self):
        with tempfile.TemporaryDirectory() as directory:
            helper = self.make_helper(FakeLoopbackBackend(), directory)
            response = helper.handle(json.dumps({"version": 1, "operation": "probe"}).encode())
            self.assertTrue(response["ok"])
            for request in (
                {"version": 2, "operation": "probe"},
                {"version": 1, "operation": "shell"},
                {"version": 1, "operation": "probe", "path": "/etc/passwd"},
                {"version": 1, "operation": "probe", "secret": "do-not-return"},
            ):
                with self.subTest(request=request):
                    response = helper.handle(json.dumps(request).encode())
                    self.assertFalse(response["ok"])
                    self.assertNotIn("do-not-return", json.dumps(response))
            invalid_mapping = valid_request()
            invalid_mapping["mapping"]["devices"][0]["backstorePath"] = "/var/lib/holo"
            self.assertFalse(helper.handle(json.dumps(invalid_mapping).encode())["ok"])
            self.assertFalse(helper.handle(b" " * (64 * 1024 + 1))["ok"])

    def test_conflicting_unowned_object_is_refused_without_mutation(self):
        with tempfile.TemporaryDirectory() as directory:
            backend = FakeLoopbackBackend()
            backend.conflict = True
            helper = self.make_helper(backend, directory)
            response = helper.handle(json.dumps(valid_request()).encode())
            self.assertFalse(response["ok"])
            self.assertEqual(backend.created, [])
            self.assertEqual(backend.removed, [])

    def test_failed_ensure_rolls_back_only_new_objects_in_reverse_order(self):
        with tempfile.TemporaryDirectory() as directory:
            backend = FakeLoopbackBackend()
            backend.present.add("target")
            backend.fail_component = "nexus"
            helper = self.make_helper(backend, directory)
            response = helper.handle(json.dumps(valid_request()).encode())
            self.assertFalse(response["ok"])
            self.assertEqual(backend.removed, ["tpg"])
            self.assertEqual(backend.present, {"target"})
            self.assertNotIn("secret=do-not-return", json.dumps(response))

    def test_one_device_ensure_failure_keeps_shared_path_and_other_devices(self):
        with tempfile.TemporaryDirectory() as directory:
            backend = FakeLoopbackBackend()
            backend.fail_component = "lun:5"
            helper = self.make_helper(backend, directory)
            response = helper.handle(json.dumps(valid_request()).encode())
            self.assertTrue(response["ok"])
            self.assertEqual(backend.present, {"target", "tpg", "nexus", "lun:0"})
            self.assertEqual(backend.removed, [])
            drive = next(device for device in response["result"] if device["deviceKey"] == "drive:drive-a")
            self.assertEqual(drive["state"], "failed")

    def test_remove_is_ordered_and_idempotent(self):
        with tempfile.TemporaryDirectory() as directory:
            backend = FakeLoopbackBackend()
            backend.present.update({"target", "tpg", "nexus", "lun:0", "lun:5"})
            helper = self.make_helper(backend, directory)
            request = json.dumps(valid_request("remove")).encode()
            self.assertTrue(helper.handle(request)["ok"])
            self.assertEqual(backend.removed, ["lun:0", "lun:5", "nexus", "tpg", "target"])
            self.assertTrue(helper.handle(request)["ok"])
            self.assertEqual(backend.removed, ["lun:0", "lun:5", "nexus", "tpg", "target"])

    def test_owned_list_includes_only_validated_device_mapping_for_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            backend = helper_module.RTSlibLoopbackBackend.__new__(helper_module.RTSlibLoopbackBackend)
            backend.owner_dir = Path(directory) / "owners"
            backend.owner_dir.mkdir()
            backend.configfs_root = Path(directory) / "configfs"
            mapping = helper_module.validate_mapping(valid_request()["mapping"], "ensure")
            (backend.configfs_root / mapping["targetNaa"]).mkdir(parents=True)
            (backend.owner_dir / "owner.json").touch()
            owner = {"version": 1, **backend._owner_record(mapping)}

            with patch.object(backend, "_read_owner_file", return_value=owner):
                listed = backend.list_owned()

            self.assertEqual(len(listed), 1)
            self.assertTrue(listed[0]["present"])
            self.assertEqual(listed[0]["libraryId"], mapping["libraryId"])
            self.assertEqual(
                [(device["deviceKey"], device["lun"], device["state"]) for device in listed[0]["devices"]],
                [("changer:library-a", 0, "cleanup_pending"), ("drive:drive-a", 5, "cleanup_pending")],
            )

    def test_owned_list_refuses_marker_missing_device_cleanup_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            backend = helper_module.RTSlibLoopbackBackend.__new__(helper_module.RTSlibLoopbackBackend)
            backend.owner_dir = Path(directory) / "owners"
            backend.owner_dir.mkdir()
            (backend.owner_dir / "owner.json").touch()
            owner = {
                "version": 1,
                "libraryId": "library-a",
                "targetNaa": "naa.50014056b18af0f5",
                "nexusNaa": "naa.5001405db2f4505b",
                "tpgTag": 1,
            }

            with patch.object(backend, "_read_owner_file", return_value=owner):
                with self.assertRaises(helper_module.HelperFailure) as raised:
                    backend.list_owned()

            self.assertEqual(raised.exception.reason, "mapping_conflict")

    def test_owned_list_refuses_unmarked_loopback_target_referencing_holo_backstore(self):
        with tempfile.TemporaryDirectory() as directory:
            backend = helper_module.RTSlibLoopbackBackend.__new__(helper_module.RTSlibLoopbackBackend)
            backend.owner_dir = Path(directory) / "owners"
            backend.configfs_root = Path(directory) / "configfs"
            target = backend.configfs_root / "naa.50014056b18af0f5"
            lun = target / "tpgt_1" / "lun" / "lun_3"
            lun.mkdir(parents=True)
            storage = Path(directory) / "backstores" / "user" / "holo_orphaned"
            storage.mkdir(parents=True)
            (lun / "holo-backstore").symlink_to(storage)

            with self.assertRaises(helper_module.HelperFailure) as raised:
                backend.list_owned()

            self.assertEqual(raised.exception.reason, "mapping_conflict")

    def test_one_device_remove_failure_does_not_block_other_luns(self):
        with tempfile.TemporaryDirectory() as directory:
            backend = FakeLoopbackBackend()
            backend.present.update({"target", "tpg", "nexus", "lun:0", "lun:5"})
            backend.fail_remove_component = "lun:0"
            helper = self.make_helper(backend, directory)
            response = helper.handle(json.dumps(valid_request("remove")).encode())
            self.assertTrue(response["ok"])
            self.assertEqual(backend.present, {"target", "tpg", "nexus", "lun:0"})
            self.assertEqual(backend.removed, ["lun:5"])
            changer = next(device for device in response["result"] if device["deviceKey"] == "changer:library-a")
            self.assertEqual(changer["state"], "residual")
            backend.fail_remove_component = None
            self.assertTrue(helper.handle(json.dumps(valid_request("remove")).encode())["ok"])
            self.assertEqual(backend.present, set())

    def test_mutations_use_exclusive_lock_and_report_lock_timeout_safely(self):
        with tempfile.TemporaryDirectory() as directory:
            backend = FakeLoopbackBackend()
            backend.delay = 0.05
            helper = self.make_helper(backend, directory, lock_timeout=0.5)
            request = json.dumps(valid_request()).encode()
            responses = []
            second = threading.Thread(target=lambda: responses.append(helper.handle(request)))
            first_result = []
            first = threading.Thread(target=lambda: first_result.append(helper.handle(request)))
            first.start()
            time.sleep(0.01)
            second.start()
            first.join()
            second.join()
            self.assertEqual(backend.max_active, 1)
            self.assertTrue(first_result[0]["ok"])
            self.assertTrue(responses[0]["ok"])

            lock_file = Path(directory) / "helper.lock"
            with lock_file.open("a+") as locked:
                fcntl.flock(locked.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                response = helper.handle(request)
                self.assertFalse(response["ok"])
                self.assertNotIn("Traceback", json.dumps(response))

if __name__ == "__main__":
    unittest.main()
