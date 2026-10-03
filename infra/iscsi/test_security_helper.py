import importlib.util
import json
import pathlib
import unittest
from unittest import mock


HELPER_PATH = pathlib.Path(__file__).with_name("holo-iscsi-security-helper.py")
SPEC = importlib.util.spec_from_file_location("holo_iscsi_security_helper", HELPER_PATH)
helper = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(helper)


class FakeStorage:
    def __init__(self, name, plugin):
        self.name = name
        self.plugin = plugin


class FakeFabric:
    def __init__(self):
        self.targets = []


class FakeTarget:
    def __init__(self, fabric, wwn, mode="create"):
        self.fabric_module = fabric
        self.wwn = wwn
        self.tpgs = []
        fabric.targets.append(self)

    def delete(self):
        self.fabric_module.targets.remove(self)


class FakeTPG:
    def __init__(self, target, tag=1, mode="create"):
        self.target = target
        self.tag = tag
        self._enable = False
        self.enable_history = []
        self.enabled_state = []
        self.attributes = {}
        self._network_portals = []
        self._node_acls = []
        self._luns = []
        target.tpgs.append(self)

    @property
    def enable(self):
        return self._enable

    @enable.setter
    def enable(self, value):
        self._enable = value
        self.enable_history.append(value)
        if value:
            self.enabled_state.append((len(self._network_portals), len(self._luns), len(self._node_acls)))

    @property
    def network_portals(self):
        return (portal for portal in self._network_portals)

    @property
    def node_acls(self):
        return (acl for acl in self._node_acls)

    @property
    def luns(self):
        return (lun for lun in self._luns)

    def set_attribute(self, name, value):
        self.attributes[name] = value


class FakeLUN:
    def __init__(self, tpg, lun, storage_object):
        self.tpg = tpg
        self.lun = lun
        self.storage_object = storage_object
        tpg._luns.append(self)


class FakeACL:
    def __init__(self, tpg, node_wwn, mode="create"):
        self.node_wwn = node_wwn
        self.mapped_luns = []
        self.chap_userid = ""
        self.chap_password = ""
        self.chap_mutual_userid = ""
        self.chap_mutual_password = ""
        tpg._node_acls.append(self)


class FakeMappedLUN:
    def __init__(self, acl, mapped_lun, tpg_lun):
        self.mapped_lun = mapped_lun
        self.tpg_lun = tpg_lun
        acl.mapped_luns.append(self)


class FakePortal:
    def __init__(self, tpg, ip_address, port, mode="create"):
        self.ip_address = ip_address
        self.port = port
        tpg._network_portals.append(self)


class FakeRTSLib:
    def __init__(self):
        self.fabric = FakeFabric()
        self.storage_objects = [FakeStorage("holo_drive_a", "fileio"), FakeStorage("holo_changer_a", "user")]

    def RTSRoot(self):
        return self

    def FabricModule(self, _name):
        return self.fabric

    Target = FakeTarget
    TPG = FakeTPG
    LUN = FakeLUN
    NodeACL = FakeACL
    MappedLUN = FakeMappedLUN
    NetworkPortal = FakePortal


class SecurityHelperProtocolTests(unittest.TestCase):
    def test_fixed_operations_and_safe_response(self):
        response = helper.process_bytes(b'{"version":1,"operation":"capabilities"}')
        self.assertEqual({"ok": True, "code": "ok", "revision": "holo-iscsi-security-v1"}, response)
        rejected = helper.process_bytes(b'{"version":1,"operation":"exec","command":"id"}')
        self.assertEqual({"ok": False, "code": "invalid_request"}, rejected)

    def test_rejects_duplicate_keys_unknown_fields_and_large_requests(self):
        duplicate = helper.process_bytes(b'{"version":1,"version":1,"operation":"capabilities"}')
        self.assertFalse(duplicate["ok"])
        extra = helper.process_bytes(b'{"version":1,"operation":"capabilities","path":"/etc/passwd"}')
        self.assertFalse(extra["ok"])
        oversized = helper.process_bytes(b" " * (helper.MAX_INPUT + 1))
        self.assertEqual("request_too_large", oversized["code"])

    def test_endpoint_rejects_wildcard_loopback_ipv6_and_out_of_range_ports(self):
        for address, port in (
            ("0.0.0.0", 3262),
            ("127.0.0.1", 3262),
            ("::1", 3262),
            ("192.0.2.10", 1023),
            ("192.0.2.10", 65536),
            ("192.0.2.10", True),
        ):
            with self.subTest(address=address, port=port), self.assertRaises(helper.InvalidRequest):
                helper._endpoint(address, port)
        helper._endpoint("192.0.2.10", 3262)

    def test_secret_material_is_never_echoed(self):
        sentinel = "secret-canary-very-long"
        request = {"version": 1, "operation": "create-target-protected", "targetIQN": "iqn.2026-04.cloud.backupnext.holo:drive-a", "backstoreName": "holo_drive_a", "backstoreType": "fileio", "endpoint": {"address": "192.0.2.2", "port": 3260}, "auth": {"mode": "chap", "username": "backup-user", "secret": sentinel}, "initiators": ["iqn.2026-04.example.test:backup:host-a"]}
        with mock.patch.object(helper, "_create_protected_target") as create_target:
            response = helper.process_bytes(json.dumps(request).encode())
            create_target.assert_called_once()
        self.assertNotIn(sentinel, json.dumps(response))

    def test_protected_target_uses_exact_acl_chap_and_endpoint(self):
        fake = FakeRTSLib()
        secret = "private-chap-secret"
        request = {"version": 1, "operation": "create-target-protected", "targetIQN": "iqn.2026-04.cloud.backupnext.holo:drive-a", "backstoreName": "holo_drive_a", "backstoreType": "fileio", "endpoint": {"address": "192.0.2.2", "port": 3260}, "auth": {"mode": "chap", "username": "backup-user", "secret": secret}, "initiators": ["iqn.1991-05.com.microsoft:backup-a"]}
        with mock.patch.object(helper, "_rtslib", return_value=fake):
            response = helper.process_bytes(json.dumps(request).encode())
        self.assertTrue(response["ok"])
        self.assertNotIn(secret, json.dumps(response))
        target = fake.fabric.targets[0]
        tpg = target.tpgs[0]
        self.assertTrue(tpg.enable)
        self.assertEqual({"authentication": 1, "generate_node_acls": 0, "cache_dynamic_acls": 0}, tpg.attributes)
        self.assertEqual([("192.0.2.2", 3260)], [(portal.ip_address, portal.port) for portal in tpg.network_portals])
        self.assertEqual(["iqn.1991-05.com.microsoft:backup-a"], [acl.node_wwn for acl in tpg.node_acls])
        self.assertEqual(("backup-user", secret), (list(tpg.node_acls)[0].chap_userid, list(tpg.node_acls)[0].chap_password))
        self.assertEqual([(0, 0)], [(mapping.mapped_lun, mapping.tpg_lun) for mapping in list(tpg.node_acls)[0].mapped_luns])
        self.assertEqual([False, True], tpg.enable_history)
        self.assertEqual([(1, 1, 1)], tpg.enabled_state)

    def test_protected_mutual_chap_uses_distinct_forward_and_reverse_pairs(self):
        fake = FakeRTSLib()
        request = {"version": 1, "operation": "create-target-protected", "targetIQN": "iqn.2026-04.cloud.backupnext.holo:drive-mutual", "backstoreName": "holo_drive_a", "backstoreType": "fileio", "endpoint": {"address": "192.0.2.2", "port": 3260}, "auth": {"mode": "mutual_chap", "username": "initiator-user", "secret": "forward-secret-123", "mutualUsername": "target-user", "mutualSecret": "reverse-secret-456"}, "initiators": ["iqn.1991-05.com.microsoft:backup-a"]}
        with mock.patch.object(helper, "_rtslib", return_value=fake):
            response = helper.process_bytes(json.dumps(request).encode())
        self.assertTrue(response["ok"])
        acl = list(fake.fabric.targets[0].tpgs[0].node_acls)[0]
        self.assertEqual(("initiator-user", "forward-secret-123"), (acl.chap_userid, acl.chap_password))
        self.assertEqual(("target-user", "reverse-secret-456"), (acl.chap_mutual_userid, acl.chap_mutual_password))

    def test_target_setup_failure_deletes_incomplete_configfs_target(self):
        fake = FakeRTSLib()
        request = {"version": 1, "operation": "create-target-protected", "targetIQN": "iqn.2026-04.cloud.backupnext.holo:drive-failed", "backstoreName": "holo_drive_a", "backstoreType": "fileio", "endpoint": {"address": "192.0.2.2", "port": 3260}, "auth": {"mode": "chap", "username": "backup-user", "secret": "private-chap-secret"}, "initiators": ["iqn.1991-05.com.microsoft:backup-a"]}
        with mock.patch.object(fake, "NetworkPortal", side_effect=RuntimeError("injected failure")), mock.patch.object(helper, "_rtslib", return_value=fake):
            response = helper.process_bytes(json.dumps(request).encode())
        self.assertEqual({"ok": False, "code": "operation_failed"}, response)
        self.assertEqual([], fake.fabric.targets)

    def test_acl_only_mode_disables_dynamic_acl_and_chap(self):
        fake = FakeRTSLib()
        request = {"version": 1, "operation": "create-target-protected", "targetIQN": "iqn.2026-04.cloud.backupnext.holo:changer-a", "backstoreName": "holo_changer_a", "backstoreType": "user:holo", "endpoint": {"address": "198.51.100.4", "port": 3260}, "auth": {"mode": "none"}, "initiators": ["iqn.1991-05.com.microsoft:backup-a"]}
        with mock.patch.object(helper, "_rtslib", return_value=fake):
            response = helper.process_bytes(json.dumps(request).encode())
        self.assertTrue(response["ok"])
        tpg = fake.fabric.targets[0].tpgs[0]
        self.assertEqual({"authentication": 0, "generate_node_acls": 0, "cache_dynamic_acls": 0}, tpg.attributes)
        self.assertEqual(1, len(list(tpg.node_acls)))
        self.assertEqual([""], [acl.chap_userid for acl in tpg.node_acls])

    def test_explicit_deny_all_acl_keeps_dynamic_acls_disabled(self):
        fake = FakeRTSLib()
        request = {"version": 1, "operation": "create-target-protected", "targetIQN": "iqn.2026-04.cloud.backupnext.holo:changer-a", "backstoreName": "holo_changer_a", "backstoreType": "user:holo", "endpoint": {"address": "198.51.100.4", "port": 3260}, "auth": {"mode": "none", "restrictInitiators": True}, "initiators": []}
        with mock.patch.object(helper, "_rtslib", return_value=fake):
            response = helper.process_bytes(json.dumps(request).encode())
        self.assertTrue(response["ok"])
        tpg = fake.fabric.targets[0].tpgs[0]
        self.assertEqual({"authentication": 0, "generate_node_acls": 0, "cache_dynamic_acls": 0}, tpg.attributes)
        self.assertEqual([], list(tpg.node_acls))


if __name__ == "__main__":
    unittest.main()
