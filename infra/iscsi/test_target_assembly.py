import json
import unittest
from unittest import mock

from infra.iscsi import test_security_helper as shared

helper = shared.helper


def protected_request(target_iqn):
    return {
        "version": 1,
        "operation": "create-target-protected",
        "targetIQN": target_iqn,
        "backstoreName": "holo_drive_a",
        "backstoreType": "fileio",
        "endpoint": {"address": "192.0.2.10", "port": 3260},
        "auth": {"mode": "chap", "username": "backup-user", "secret": "private-chap-secret"},
        "initiators": ["iqn.1991-05.com.microsoft:backup-a"],
    }


class ProtectedTargetAssemblyTests(unittest.TestCase):
    def test_tpg_is_enabled_only_after_explicit_acl_lun_and_portal_readback(self):
        fake = shared.FakeRTSLib()
        request = protected_request("iqn.2026-04.cloud.backupnext.holo:drive-assembly")
        with mock.patch.object(helper, "_rtslib", return_value=fake):
            response = helper.process_bytes(json.dumps(request).encode())

        self.assertTrue(response["ok"])
        tpg = fake.fabric.targets[0].tpgs[0]
        self.assertEqual([False, True], tpg.enable_history)
        self.assertEqual([(1, 1, 1)], tpg.enabled_state)
        self.assertEqual([("192.0.2.10", 3260)], [(portal.ip_address, portal.port) for portal in tpg.network_portals])
        self.assertEqual(["iqn.1991-05.com.microsoft:backup-a"], [acl.node_wwn for acl in tpg.node_acls])
        acl = list(tpg.node_acls)[0]
        self.assertEqual([(0, 0)], [(mapping.mapped_lun, mapping.tpg_lun) for mapping in acl.mapped_luns])

    def test_each_target_setup_failure_removes_the_incomplete_target(self):
        for failing_component in ("TPG", "LUN", "NodeACL", "NetworkPortal"):
            with self.subTest(component=failing_component):
                fake = shared.FakeRTSLib()
                request = protected_request(f"iqn.2026-04.cloud.backupnext.holo:fail-{failing_component.lower()}")
                with mock.patch.object(fake, failing_component, side_effect=RuntimeError("injected setup failure")), \
                        mock.patch.object(helper, "_rtslib", return_value=fake):
                    response = helper.process_bytes(json.dumps(request).encode())
                self.assertEqual({"ok": False, "code": "operation_failed"}, response)
                self.assertEqual([], fake.fabric.targets)

    def test_enable_failure_removes_target_without_exposing_an_incomplete_tpg(self):
        class FailingEnableTPG(shared.FakeTPG):
            @property
            def enable(self):
                return super().enable

            @enable.setter
            def enable(self, value):
                if value:
                    raise RuntimeError("injected enable failure")
                shared.FakeTPG.enable.fset(self, value)

        fake = shared.FakeRTSLib()
        fake.TPG = FailingEnableTPG
        request = protected_request("iqn.2026-04.cloud.backupnext.holo:fail-enable")
        with mock.patch.object(helper, "_rtslib", return_value=fake):
            response = helper.process_bytes(json.dumps(request).encode())
        self.assertEqual({"ok": False, "code": "operation_failed"}, response)
        self.assertEqual([], fake.fabric.targets)


if __name__ == "__main__":
    unittest.main()
