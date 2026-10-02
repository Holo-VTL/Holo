import importlib.util
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


HELPER_PATH = Path(__file__).with_name("holo-local-loopback-helper.py")
spec = importlib.util.spec_from_file_location("holo_local_loopback_helper_enumeration", HELPER_PATH)
helper_module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper_module)


def valid_mapping():
    return {
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
                "state": "active",
            },
            {
                "deviceKey": "drive:drive-a",
                "kind": "drive",
                "driveId": "drive-a",
                "lun": 5,
                "identityRef": "drive-identity-a",
                "backendRef": "holo_backstore_drive_a",
                "state": "active",
            },
        ],
    }


class LocalLoopbackEnumerationTests(unittest.TestCase):
    def test_enumeration_matches_loopback_lun_vpd_and_pdt(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            hosts = root / "class" / "scsi_host"
            generic = root / "class" / "scsi_generic"
            scsi_devices = root / "bus" / "scsi" / "devices"
            tape = root / "class" / "st"
            fake_bin = root / "bin"
            fake_bin.mkdir()
            host = hosts / "host11"
            host.mkdir(parents=True)
            (host / "proc_name").write_text("tcm_loopback\n", encoding="utf-8")
            for lun, sg, pdt in ((0, "sg0", 8), (5, "sg5", 1)):
                device = scsi_devices / f"11:0:0:{lun}"
                device.mkdir(parents=True)
                (device / "type").write_text(f"{pdt}\n", encoding="ascii")
                sg_device = generic / sg / "device"
                sg_device.parent.mkdir(parents=True)
                sg_device.symlink_to(device)
                if lun == 5:
                    st_device = tape / "st0" / "device"
                    st_device.parent.mkdir(parents=True)
                    st_device.symlink_to(device)
            sg_inq = fake_bin / "sg_inq"
            sg_inq.write_text(
                "#!/bin/sh\n"
                "case \"$2\" in\n"
                "  /dev/sg0) serial=changer-identity-a ;;\n"
                "  /dev/sg5) serial=drive-identity-a ;;\n"
                "  *) exit 2 ;;\n"
                "esac\n"
                "printf 'Unit serial number: %s\\n' \"$serial\"\n",
                encoding="utf-8",
            )
            sg_inq.chmod(0o755)
            backend = helper_module.RTSlibLoopbackBackend.__new__(helper_module.RTSlibLoopbackBackend)
            with patch.object(helper_module, "SCSI_HOST_ROOT", str(hosts)), \
                 patch.object(helper_module, "SCSI_GENERIC_ROOT", str(generic)), \
                 patch.object(helper_module, "SCSI_DEVICE_ROOT", str(scsi_devices)), \
                 patch.object(helper_module, "SCSI_TAPE_ROOT", str(tape)), \
                 patch.dict(os.environ, {"PATH": str(fake_bin)}):
                mapping = helper_module.validate_mapping(valid_mapping(), "ensure")
                observed = backend.observe(mapping)

            by_key = {item["deviceKey"]: item for item in observed}
            self.assertEqual(by_key["changer:library-a"]["state"], "connected")
            self.assertEqual(by_key["changer:library-a"]["observedPaths"], ["/dev/sg0"])
            self.assertEqual(by_key["drive:drive-a"]["state"], "connected")
            self.assertEqual(by_key["drive:drive-a"]["observedPaths"], ["/dev/sg5", "/dev/st0"])

    def test_configfs_without_enumerated_loopback_host_stays_pending(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            hosts = root / "class" / "scsi_host"
            generic = root / "class" / "scsi_generic"
            scsi_devices = root / "bus" / "scsi" / "devices"
            hosts.mkdir(parents=True)
            generic.mkdir(parents=True)
            scsi_devices.mkdir(parents=True)
            backend = helper_module.RTSlibLoopbackBackend.__new__(helper_module.RTSlibLoopbackBackend)
            with patch.object(helper_module, "SCSI_HOST_ROOT", str(hosts)), \
                 patch.object(helper_module, "SCSI_GENERIC_ROOT", str(generic)), \
                 patch.object(helper_module, "SCSI_DEVICE_ROOT", str(scsi_devices)):
                mapping = helper_module.validate_mapping(valid_mapping(), "ensure")
                observed = backend.observe(mapping)
            self.assertEqual({item["state"] for item in observed}, {"pending"})
            self.assertEqual({item["reasonCode"] for item in observed}, {"device_not_enumerated"})


if __name__ == "__main__":
    unittest.main()
