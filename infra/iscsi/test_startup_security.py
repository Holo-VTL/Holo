import json
import grp
import os
import pathlib
import stat
import tempfile
import unittest
from unittest import mock

from infra.iscsi import test_security_helper as shared

helper = shared.helper


def target(iqn, *, attributes=None, acls=None, portals=None):
    return {
        "fabric": "iscsi",
        "wwn": iqn,
        "tpgs": [{
            "attributes": attributes or {},
            "node_acls": acls or [],
            "portals": portals or [],
        }],
    }


class SavedTargetConfigTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.path = os.path.join(self.directory.name, "saveconfig.json")
        self.addCleanup(self.directory.cleanup)
        self.path_patch = mock.patch.object(helper, "TARGETCLI_SAVE_CONFIG_PATH", self.path)
        self.path_patch.start()
        self.addCleanup(self.path_patch.stop)
        self.root_file_patch = mock.patch.object(
            helper,
            "_root_file",
            side_effect=lambda path, mode=None: type("Info", (), {"st_size": os.path.getsize(path)})(),
        )
        self.root_file_patch.start()
        self.addCleanup(self.root_file_patch.stop)
        self.legacy_port_patch = mock.patch.object(helper, "_configured_target_port", return_value=3260)
        self.legacy_port_patch.start()
        self.addCleanup(self.legacy_port_patch.stop)

    def write_config(self, targets):
        with open(self.path, "w", encoding="utf-8") as config_file:
            json.dump({"targets": targets}, config_file)

    def test_missing_saved_config_is_allowed(self):
        helper._check_saved_target_config()

    def test_unrelated_administrator_chap_target_is_preserved(self):
        self.write_config([target(
            "iqn.2001-04.example.admin:array",
            attributes={"authentication": 1, "generate_node_acls": 0},
            acls=[{"chap_userid": "admin-user", "chap_password": "admin-secret-canary"}],
            portals=[{"ip_address": "192.0.2.20", "port": 3262}],
        )])
        helper._check_saved_target_config()

    def test_open_legacy_holo_target_is_allowed(self):
        self.write_config([target(
            "iqn.2026-04.cloud.backupnext.holo:legacy",
            attributes={"authentication": 0, "generate_node_acls": 1, "cache_dynamic_acls": 1},
            portals=[{"ip_address": "192.0.2.20", "port": 3260}],
        )])
        helper._check_saved_target_config()

    def test_holo_chap_target_is_rejected_without_echoing_secret(self):
        secret = "chap-secret-canary"
        self.write_config([target(
            "iqn.2026-04.cloud.backupnext.holo:stale-chap",
            attributes={"authentication": 1},
            acls=[{"chap_userid": "backup-user", "chap_password": secret}],
            portals=[{"ip_address": "192.0.2.20", "port": 3260}],
        )])
        with self.assertRaises(helper.InvalidRequest) as raised:
            helper._check_saved_target_config()
        self.assertNotIn(secret, str(raised.exception))
        response = helper.process_bytes(b'{"version":1,"operation":"startup-check"}')
        self.assertEqual({"ok": False, "code": "operation_failed"}, response)
        self.assertNotIn(secret, json.dumps(response))

    def test_holo_dynamic_acl_target_is_rejected(self):
        self.write_config([target(
            "iqn.2026-04.cloud.backupnext.holo:stale-acl",
            attributes={"authentication": 0, "generate_node_acls": 0},
            portals=[{"ip_address": "192.0.2.20", "port": 3260}],
        )])
        with self.assertRaises(helper.InvalidRequest):
            helper._check_saved_target_config()

    def test_empty_vault_key_is_created_private_and_with_holo_group(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            env_path = root / "holo.env"
            key_path = root / "iscsi-secrets.key"
            database_path = root / "missing.db"
            env_path.write_text(f"HOLO_METADATA_DSN={database_path}\nHOLO_ISCSI_SECRET_KEY={key_path}\n", encoding="utf-8")
            info = type("Info", (), {"st_uid": 0, "st_mode": stat.S_IFDIR | 0o700})()
            real_lstat = os.lstat

            def lstat(path):
                if path == str(root):
                    return info
                if path in (str(database_path), str(key_path)):
                    raise FileNotFoundError(path)
                return real_lstat(path)

            with mock.patch.object(helper, "HOLO_ENV_PATH", str(env_path)), \
                    mock.patch.object(helper, "_root_file"), \
                    mock.patch.object(helper.os, "lstat", side_effect=lstat), \
                    mock.patch.object(helper.os, "fchown") as fchown, \
                    mock.patch.object(helper.secrets, "token_bytes", return_value=b"k" * 32), \
                    mock.patch.object(grp, "getgrnam", return_value=type("Entry", (), {"gr_gid": 4242})()):
                helper._provision_empty_vault_key()

            self.assertEqual(b"k" * 32, key_path.read_bytes())
            self.assertEqual(0o640, stat.S_IMODE(key_path.stat().st_mode))
            self.assertEqual((0, 4242), fchown.call_args.args[1:])

    def test_holo_nonlegacy_port_target_is_rejected(self):
        self.write_config([target(
            "iqn.2026-04.cloud.backupnext.holo:stale-secure-port",
            attributes={"authentication": 0, "generate_node_acls": 1, "cache_dynamic_acls": 1},
            portals=[{"ip_address": "192.0.2.20", "port": 3262}],
        )])
        with self.assertRaises(helper.InvalidRequest):
            helper._check_saved_target_config()


if __name__ == "__main__":
    unittest.main()
