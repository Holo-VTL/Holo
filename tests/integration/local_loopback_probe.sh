#!/usr/bin/env bash
set -euo pipefail

exec python3 - "$@" <<'PY'
from __future__ import annotations

import argparse
import copy
import glob
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path


FIXTURE_PREFIX = "codex053probe-"
PORTAL_BASE = "http://127.0.0.1"
INITIATOR_IQN = "iqn.1991-05.com.microsoft:win-6glm8jlbgp9"
LUN_IDS = {"changer": 0, "drive01": 5, "drive02": 9}
MARKER_DIR = Path("/run/holo/local-loopback-probe")


class ProbeError(RuntimeError):
    pass


@dataclass(frozen=True)
class Plan:
    library_id: str
    target_wwn: str
    nexus_wwn: str
    luns: dict[int, str]

    def marker(self) -> dict[str, object]:
        return {
            "protocol": 1,
            "libraryId": self.library_id,
            "targetWWN": self.target_wwn,
            "nexusWWN": self.nexus_wwn,
            "luns": {str(k): v for k, v in sorted(self.luns.items())},
        }


def fail(message: str) -> None:
    raise ProbeError(message)


def call_json(path: str) -> object:
    request = urllib.request.Request(PORTAL_BASE + path, headers={"Accept": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=5) as response:
            return json.loads(response.read(1024 * 1024))
    except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as exc:
        raise ProbeError(f"control-plane read failed for {path}: {type(exc).__name__}") from None


def make_plan(library_id: str) -> tuple[Plan, dict[str, dict[str, object]]]:
    if not library_id.startswith(FIXTURE_PREFIX) or not re.fullmatch(r"[a-z0-9-]{10,64}", library_id):
        fail(f"libraryId must use the isolated {FIXTURE_PREFIX} fixture prefix")

    rows = call_json("/v1/targets/publications")
    if not isinstance(rows, list):
        fail("publication inventory has an invalid shape")
    fixture = [r for r in rows if isinstance(r, dict) and r.get("libraryId") == library_id]
    changer_rows = [r for r in fixture if r.get("deviceRole") == "changer"]
    drive_rows = sorted((r for r in fixture if r.get("deviceRole") == "drive"), key=lambda r: str(r.get("driveId")))
    if len(changer_rows) != 1 or len(drive_rows) != 2:
        fail("probe fixture must have exactly one changer and two published drives")

    changer = changer_rows[0]
    drives = drive_rows
    if drives[0].get("driveId") != library_id + "-drv-01" or drives[1].get("driveId") != library_id + "-drv-02":
        fail("probe drives do not match the isolated fixture names")

    library_security = call_json(f"/v1/libraries/{library_id}/iscsi-security")
    binding = library_security.get("binding", {}) if isinstance(library_security, dict) else {}
    auth = binding.get("auth", {}) if isinstance(binding, dict) else {}
    if auth.get("mode") != "mutual_chap" or INITIATOR_IQN not in auth.get("initiators", []):
        fail("probe fixture is not restricted to its mutual-CHAP test initiator")

    local_mount = call_json("/v1/targets/local-mount")
    if not isinstance(local_mount, dict) or local_mount.get("enabled") is not False:
        fail("the product local-mount switch must remain disabled during G0-A")

    backstores: dict[str, dict[str, object]] = {}
    lun_map: dict[int, str] = {}
    for row, lun_id in ((changer, LUN_IDS["changer"]), (drives[0], LUN_IDS["drive01"]), (drives[1], LUN_IDS["drive02"])):
        publication_id = str(row.get("publicationId", ""))
        target_iqn = str(row.get("targetIqn", ""))
        role = str(row.get("deviceRole", ""))
        if not publication_id.startswith("pub-") or not target_iqn.startswith("iqn."):
            fail("fixture publication identity is invalid")
        storage_suffix = re.sub(r"[^a-z0-9]", "_", publication_id.lower()).strip("_")[:48]
        backstore = "holo_" + storage_suffix
        if not backstore.startswith("holo_pub_"):
            fail("fixture backstore name is outside the Holo publication namespace")
        backstores[backstore] = {"targetIQN": target_iqn, "role": role, "lun": lun_id}
        lun_map[lun_id] = "user:" + backstore

    digest = hashlib.sha256(library_id.encode("ascii")).hexdigest()
    target_wwn = "naa.5001405" + digest[:9]
    nexus_wwn = "naa.5001405" + hashlib.sha256((library_id + ":nexus").encode("ascii")).hexdigest()[:9]
    return Plan(library_id, target_wwn, nexus_wwn, lun_map), backstores


def validate_partial(plan: Plan, info: dict[str, object] | None) -> None:
    if info is None:
        return
    if info.get("nexus") not in (None, plan.nexus_wwn):
        fail("existing loopback nexus does not match this probe's ownership record")
    luns = info.get("luns", {})
    if not isinstance(luns, dict):
        fail("existing loopback LUN inventory is invalid")
    for lun_text, backstore in luns.items():
        try:
            lun_id = int(lun_text)
        except (TypeError, ValueError):
            fail("existing loopback target has an unexpected LUN")
        if plan.luns.get(lun_id) != backstore:
            fail("existing loopback target contains a LUN outside this probe's plan")


def reconcile(ops: object, plan: Plan) -> None:
    owner = ops.read_owner(plan)
    info = ops.inspect(plan)
    if info is not None and owner != plan.marker():
        fail("refusing to modify a loopback target without this probe's ownership marker")
    if owner is not None and owner != plan.marker():
        fail("probe ownership marker does not match the requested fixture")
    validate_partial(plan, info)
    if owner is None:
        ops.write_owner(plan)
    try:
        if info is None:
            ops.create_target(plan)
            info = ops.inspect(plan)
        if info is None or info.get("nexus") is None:
            ops.create_tpg(plan)
            info = ops.inspect(plan)
        if info is None or info.get("nexus") != plan.nexus_wwn:
            fail("loopback nexus was not initialized with the stable probe identity")
        present = info.get("luns", {})
        if not isinstance(present, dict):
            fail("loopback LUN inventory is invalid")
        for lun_id, backstore in sorted(plan.luns.items()):
            if str(lun_id) not in present:
                ops.add_lun(plan, lun_id, backstore)
        info = ops.inspect(plan)
        validate_partial(plan, info)
        expected = {str(k): v for k, v in sorted(plan.luns.items())}
        if info is None or info.get("nexus") != plan.nexus_wwn or info.get("luns") != expected:
            fail("loopback target did not converge to the exact probe mapping")
    except Exception:
        if ops.read_owner(plan) == plan.marker():
            try:
                remove_probe(ops, plan)
            except Exception as cleanup_error:
                raise ProbeError(f"probe failed and owned-object cleanup is still pending: {type(cleanup_error).__name__}") from None
        raise


def remove_probe(ops: object, plan: Plan) -> None:
    owner = ops.read_owner(plan)
    info = ops.inspect(plan)
    if info is None and owner is None:
        return
    if owner != plan.marker():
        fail("refusing to remove a loopback target without this probe's ownership marker")
    if info is not None:
        validate_partial(plan, info)
        ops.delete_target(plan)
        if ops.inspect(plan) is not None:
            fail("owned loopback target remains after cleanup")
    ops.clear_owner(plan)


class FakeOps:
    def __init__(self, fail_at: int | None = None) -> None:
        self.owner: dict[str, object] | None = None
        self.target: dict[str, object] | None = None
        self.fail_at = fail_at
        self.created_luns = 0
        self.remove_calls = 0

    def read_owner(self, plan: Plan) -> dict[str, object] | None:
        return copy.deepcopy(self.owner)

    def write_owner(self, plan: Plan) -> None:
        self.owner = plan.marker()

    def clear_owner(self, plan: Plan) -> None:
        self.owner = None

    def inspect(self, plan: Plan) -> dict[str, object] | None:
        return copy.deepcopy(self.target)

    def create_target(self, plan: Plan) -> None:
        if self.target is not None:
            fail("fake target collision")
        self.target = {"nexus": None, "luns": {}}

    def create_tpg(self, plan: Plan) -> None:
        if self.target is None:
            fail("fake target missing")
        self.target["nexus"] = plan.nexus_wwn

    def add_lun(self, plan: Plan, lun_id: int, backstore: str) -> None:
        self.created_luns += 1
        if self.fail_at == lun_id:
            fail("injected fake LUN creation failure")
        if self.target is None:
            fail("fake target missing")
        self.target["luns"][str(lun_id)] = backstore

    def remove_owned(self, plan: Plan) -> None:
        self.remove_calls += 1
        self.target = None

    def delete_target(self, plan: Plan) -> None:
        self.remove_owned(plan)


def run_self_test() -> None:
    plan = Plan(FIXTURE_PREFIX + "selftest", "naa.6001405deadbeef01", "naa.6001405deadbeef02", {0: "user:holo_pub_test_changer", 5: "user:holo_pub_test_drive1", 9: "user:holo_pub_test_drive2"})

    fake = FakeOps()
    reconcile(fake, plan)
    first_created_count = fake.created_luns
    reconcile(fake, plan)
    assert fake.created_luns == first_created_count, "repeat ensure must be idempotent"
    assert fake.target["luns"] == {"0": "user:holo_pub_test_changer", "5": "user:holo_pub_test_drive1", "9": "user:holo_pub_test_drive2"}
    remove_probe(fake, plan)
    remove_probe(fake, plan)
    assert fake.target is None and fake.owner is None, "owned cleanup must remove only this probe"
    assert fake.remove_calls == 1, "repeat remove must be an idempotent no-op"

    failed = FakeOps(fail_at=9)
    try:
        reconcile(failed, plan)
    except ProbeError:
        pass
    else:
        raise AssertionError("injected create failure must fail the probe")
    assert failed.target is None and failed.owner is None, "failed creation must rollback only its partial target"

    foreign = FakeOps()
    foreign.target = {"nexus": plan.nexus_wwn, "luns": {"0": "holo_pub_foreign"}}
    original = copy.deepcopy(foreign.target)
    try:
        reconcile(foreign, plan)
    except ProbeError:
        pass
    else:
        raise AssertionError("unowned target must be refused")
    assert foreign.target == original and foreign.remove_calls == 0, "foreign target must remain untouched"

    print("PASS: ownership refusal")
    print("PASS: failure rollback")
    print("PASS: idempotent ensure and remove")


class RealOps:
    def __init__(self, backstores: dict[str, dict[str, object]]) -> None:
        if os.geteuid() != 0:
            fail("--run requires root on the Linux Holo host")
        try:
            from rtslib import FabricModule, LUN, RTSRoot, TPG, Target
        except ImportError:
            fail("rtslib-fb is unavailable; refusing to make target changes")
        self.FabricModule = FabricModule
        self.LUN = LUN
        self.RTSRoot = RTSRoot
        self.TPG = TPG
        self.Target = Target
        self.backstores = backstores
        self.storage_objects = {}
        for storage in RTSRoot().storage_objects:
            if storage.plugin == "user":
                self.storage_objects[storage.name] = storage
        for name, metadata in backstores.items():
            storage = self.storage_objects.get(name)
            if storage is None or storage.status != "activated":
                fail("fixture Holo backstore is absent or inactive")
            if metadata.get("targetIQN") not in [
                lun.parent_tpg.parent_target.wwn
                for lun in storage.attached_luns
                if lun.parent_tpg.parent_target.fabric_module.name == "iscsi"
            ]:
                fail("fixture Holo backstore is not attached to its expected iSCSI target")

    def marker_path(self, plan: Plan) -> Path:
        return MARKER_DIR / (plan.library_id + ".json")

    def target_path(self, plan: Plan) -> Path:
        return Path("/sys/kernel/config/target/loopback") / plan.target_wwn

    def read_owner(self, plan: Plan) -> dict[str, object] | None:
        path = self.marker_path(plan)
        try:
            return json.loads(path.read_text(encoding="utf-8"))
        except FileNotFoundError:
            return None
        except (OSError, json.JSONDecodeError):
            fail("probe ownership record is unreadable")

    def write_owner(self, plan: Plan) -> None:
        MARKER_DIR.mkdir(mode=0o700, parents=True, exist_ok=True)
        os.chmod(MARKER_DIR, 0o700)
        path = self.marker_path(plan)
        payload = json.dumps(plan.marker(), sort_keys=True, separators=(",", ":"))
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
        try:
            fd = os.open(path, flags, 0o600)
        except FileExistsError:
            fail("probe ownership record already exists; inspect before retrying")
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            stream.write(payload)
            stream.flush()
            os.fsync(stream.fileno())

    def clear_owner(self, plan: Plan) -> None:
        try:
            self.marker_path(plan).unlink()
        except FileNotFoundError:
            pass

    def _target(self, plan: Plan):
        if not self.target_path(plan).is_dir():
            return None
        fabric = self.FabricModule("loopback")
        return self.Target(fabric, plan.target_wwn, mode="lookup")

    def inspect(self, plan: Plan) -> dict[str, object] | None:
        target_path = self.target_path(plan)
        if not target_path.is_dir():
            return None
        tpg_path = target_path / "tpgt_1"
        if not tpg_path.is_dir():
            return {"nexus": None, "luns": {}}
        try:
            nexus = (tpg_path / "nexus").read_text(encoding="ascii").strip()
        except OSError:
            nexus = ""
        if not nexus:
            return {"nexus": None, "luns": {}}
        target = self._target(plan)
        if target is None:
            return None
        tpgs = list(target.tpgs)
        if not tpgs:
            return {"nexus": nexus, "luns": {}}
        if len(tpgs) != 1 or tpgs[0].tag != 1:
            fail("probe target contains an unexpected TPG")
        tpg = tpgs[0]
        luns = {str(lun.lun): lun.storage_object.plugin + ":" + lun.storage_object.name for lun in tpg.luns}
        return {"nexus": nexus, "luns": luns}

    def create_target(self, plan: Plan) -> None:
        if self.target_path(plan).exists():
            fail("loopback target WWN already exists; refusing to claim it")
        self.Target(self.FabricModule("loopback"), plan.target_wwn, mode="create")

    def create_tpg(self, plan: Plan) -> None:
        target = self._target(plan)
        if target is None:
            fail("probe target disappeared before TPG creation")
        tpg_path = Path(target.path) / "tpgt_1"
        if tpg_path.exists():
            try:
                existing_nexus = (tpg_path / "nexus").read_text(encoding="ascii").strip()
            except OSError:
                existing_nexus = ""
            if existing_nexus and existing_nexus != plan.nexus_wwn:
                fail("probe TPG already has a different nexus")
        else:
            os.mkdir(tpg_path, 0o755)
        try:
            try:
                current_nexus = (tpg_path / "nexus").read_text(encoding="ascii").strip()
            except OSError:
                current_nexus = ""
            if not current_nexus:
                with open(tpg_path / "nexus", "w", encoding="ascii") as nexus_file:
                    nexus_file.write(plan.nexus_wwn)
            self.TPG(target, tag=1, mode="lookup")
        except Exception:
            raise

    def add_lun(self, plan: Plan, lun_id: int, backstore: str) -> None:
        target = self._target(plan)
        if target is None:
            fail("probe target disappeared before LUN creation")
        tpg = self.TPG(target, tag=1, mode="lookup")
        if not backstore.startswith("user:"):
            fail("planned loopback LUN is not a Holo user backstore")
        storage = self.storage_objects.get(backstore.removeprefix("user:"))
        if storage is None:
            fail("planned Holo backstore is no longer present")
        self.LUN(tpg, lun=lun_id, storage_object=storage)

    def remove_owned(self, plan: Plan) -> None:
        if self.read_owner(plan) != plan.marker():
            fail("refusing to remove a loopback target without its matching owner record")
        target = self._target(plan)
        if target is None:
            return
        info = self.inspect(plan)
        validate_partial(plan, info)
        tpg_path = self.target_path(plan) / "tpgt_1"
        if tpg_path.is_dir():
            try:
                current_nexus = (tpg_path / "nexus").read_text(encoding="ascii").strip()
            except OSError:
                current_nexus = ""
            if not current_nexus:
                with open(tpg_path / "nexus", "w", encoding="ascii") as nexus_file:
                    nexus_file.write(plan.nexus_wwn)
        target.delete()

    def delete_target(self, plan: Plan) -> None:
        self.remove_owned(plan)


def require_zero_network_sessions_for_fixture(library_id: str) -> None:
    rows = call_json("/v1/targets/publications")
    fixture = [r for r in rows if isinstance(r, dict) and r.get("libraryId") == library_id]
    if len(fixture) != 3:
        fail("fixture publication inventory changed before the probe")
    for row in fixture:
        hosts = row.get("connectedHosts", {})
        if isinstance(hosts, dict) and int(hosts.get("sessionCount", 0) or 0) != 0:
            fail("a network initiator is connected to a probe fixture target")


def loopback_hcls() -> dict[int, dict[int, str]]:
    hosts: dict[int, dict[int, str]] = {}
    for host_path in Path("/sys/class/scsi_host").glob("host*"):
        host_name = host_path.name
        try:
            proc_name = (host_path / "proc_name").read_text(encoding="utf-8").strip()
        except OSError:
            continue
        if proc_name not in ("tcm_loopback", "tcm_loop"):
            continue
        host_id = int(host_name[4:])
        hosts[host_id] = {}
    for sg_path in Path("/sys/class/scsi_generic").glob("sg*"):
        try:
            hctl = Path(os.path.realpath(sg_path / "device")).name
            host_text, _channel, _target, lun_text = hctl.split(":")
            host_id, lun_id = int(host_text), int(lun_text)
        except (OSError, ValueError):
            continue
        if host_id in hosts:
            hosts[host_id][lun_id] = "/dev/" + sg_path.name
    return hosts


def wait_for_devices(expected_luns: set[int]) -> dict[int, str]:
    for _ in range(30):
        subprocess.run(["udevadm", "settle", "--timeout=3"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        current = loopback_hcls()
        found: dict[int, str] = {}
        for devices in current.values():
            found.update(devices)
        if set(found) == expected_luns and len(current) == 1:
            return found
        time.sleep(1)
    fail("local SCSI enumeration did not converge to the expected LUN set")


def scan_created_loopback_host(lun_ids: set[int]) -> None:
    hosts = []
    for host_path in Path("/sys/class/scsi_host").glob("host*"):
        try:
            if (host_path / "proc_name").read_text(encoding="utf-8").strip() in ("tcm_loopback", "tcm_loop"):
                hosts.append(host_path)
        except OSError:
            continue
    if len(hosts) != 1:
        fail("loopback did not create exactly one local SCSI host")
    scan_path = hosts[0] / "scan"
    try:
        # This SCSI host was created by this probe and has no other target;
        # wildcard scan is confined to that isolated host.
        with open(scan_path, "w", encoding="ascii") as scan_file:
            scan_file.write("- - -\n")
    except OSError as exc:
        fail(f"isolated loopback host scan failed (errno={exc.errno})")


def inquiry_signature(sg_path: str) -> tuple[str, str, str]:
    outputs: list[str] = []
    for args in (["sg_inq", "--verbose", sg_path], ["sg_inq", "--page=0x80", sg_path], ["sg_inq", "--page=0x83", sg_path]):
        result = subprocess.run(args, text=True, capture_output=True, timeout=5)
        if result.returncode != 0:
            fail("SG_INQ_FAILED on an isolated probe device")
        outputs.append(result.stdout.strip())
    standard = outputs[0]
    match = re.search(r"Peripheral device type:\s*([^\n(]+)", standard, re.IGNORECASE)
    if not match:
        fail("standard INQUIRY did not report a peripheral type")
    reported = match.group(1).strip().lower()
    if "medium changer" in reported or reported.startswith("8 ") or reported == "8":
        pdt = "medium changer"
    elif any(token in reported for token in ("tape", "sequential")) or reported.startswith("1 ") or reported == "1":
        pdt = "tape"
    else:
        pdt = reported
    return pdt, hashlib.sha256(outputs[1].encode()).hexdigest(), hashlib.sha256(outputs[2].encode()).hexdigest()


def targetcli_session_count() -> int:
    result = subprocess.run(["targetcli", "sessions"], text=True, capture_output=True, timeout=10)
    if result.returncode != 0:
        fail("could not read the current iSCSI session count")
    return sum(1 for line in result.stdout.splitlines() if re.search(r"\bsid:\s*\d+", line))


def loopback_targets(root: Path) -> list[Path]:
    if not root.is_dir():
        return []
    return [path for path in root.iterdir() if path.is_dir() and path.name.startswith("naa.")]


def run_real_probe(library_id: str) -> None:
    if os.geteuid() != 0:
        fail("run the real probe under sudo on the Holo Linux host")
    if not Path("/sys/kernel/config/target").is_dir():
        fail("LIO configfs is not mounted; no automatic mount or system change will be attempted")
    for command in ("modprobe", "lsscsi", "sg_inq", "udevadm", "targetcli"):
        if shutil.which(command) is None:
            fail(f"required read-only probe tool is missing: {command}")
    plan, backstores = make_plan(library_id)
    require_zero_network_sessions_for_fixture(library_id)
    sessions_before = targetcli_session_count()
    module_was_loaded = any(line.startswith("tcm_loop ") for line in Path("/proc/modules").read_text().splitlines())
    subprocess.run(["modprobe", "tcm_loop"], check=True, timeout=10)
    try:
        from rtslib import FabricModule
        FabricModule("loopback")._check_self()
    except Exception as exc:
        if not module_was_loaded:
            subprocess.run(["modprobe", "-r", "tcm_loop"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10)
        fail(f"rtslib could not initialize the loopback fabric: {type(exc).__name__}")
    loopback_root = Path("/sys/kernel/config/target/loopback")
    if not loopback_root.is_dir():
        if not module_was_loaded:
            subprocess.run(["modprobe", "-r", "tcm_loop"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10)
        fail("loopback fabric configfs is unavailable")
    if loopback_targets(loopback_root):
        if not module_was_loaded and not loopback_hcls():
            subprocess.run(["modprobe", "-r", "tcm_loop"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10)
        fail("pre-existing loopback targets were found; refusing to share or remove them")

    try:
        ops = RealOps(backstores)
        if loopback_hcls():
            fail("orphaned tcm_loop SCSI hosts were found before the probe")
    except Exception:
        if not module_was_loaded and not loopback_targets(loopback_root) and not any(loopback_hcls().values()):
            subprocess.run(["modprobe", "-r", "tcm_loop"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10)
        raise

    first_signatures: dict[int, tuple[str, str, str]] = {}
    try:
        reconcile(ops, plan)
        reconcile(ops, plan)
        configured = ops.inspect(plan)
        print(f"probe_loopback_wwn={plan.target_wwn}")
        print(f"probe_explicit_nexus={configured.get('nexus') if configured else 'missing'}")
        print(f"probe_lun_map={configured.get('luns') if configured else 'missing'}")
        attached = {name: set() for name in backstores}
        for name, storage in ops.storage_objects.items():
            if name not in attached:
                continue
            for lun in storage.attached_luns:
                attached[name].add(lun.parent_tpg.parent_target.fabric_module.name)
        for name in backstores:
            if not {"iscsi", "loopback"}.issubset(attached[name]):
                fail("same Holo user backstore is not linked through both iSCSI and loopback")

        scan_created_loopback_host(set(plan.luns))

        devices = wait_for_devices(set(plan.luns))
        lsscsi = subprocess.run(["lsscsi", "-g"], text=True, capture_output=True, timeout=10)
        if lsscsi.returncode != 0:
            fail("lsscsi failed to enumerate loopback devices")
        for device in devices.values():
            if not any(device in line for line in lsscsi.stdout.splitlines()):
                fail("lsscsi did not report every loopback sg device")
        pdt_expectations = {0: "medium changer", 5: "tape", 9: "tape"}
        for lun_id, device in sorted(devices.items()):
            signature = inquiry_signature(device)
            pdt = signature[0]
            if pdt_expectations[lun_id] not in pdt and not (lun_id > 0 and "sequential" in pdt):
                fail("INQUIRY peripheral type does not match the planned changer/drive role")
            first_signatures[lun_id] = signature
        ops.remove_owned(plan)
        for _ in range(15):
            if not any(loopback_hcls().values()):
                break
            time.sleep(1)
        if any(loopback_hcls().values()):
            fail("local SCSI devices remained after removing the owned loopback mapping")
        reconcile(ops, plan)
        scan_created_loopback_host(set(plan.luns))
        devices = wait_for_devices(set(plan.luns))
        second_signatures = {lun: inquiry_signature(path) for lun, path in sorted(devices.items())}
        if first_signatures != second_signatures:
            fail("Inquiry/VPD identity changed after loopback link recreation")
        print("PASS: the same three Holo user backstores were attached to iSCSI and loopback")
        print("PASS: non-contiguous LUNs 0/5/9 enumerated with changer/tape PDTs")
        print("PASS: stable nexus and INQUIRY/VPD signatures survived link recreation")
        for lun_id, signature in sorted(second_signatures.items()):
            print(f"LUN {lun_id}: PDT={signature[0]}; VPD80_SHA256={signature[1]}; VPD83_SHA256={signature[2]}")
    finally:
        remove_probe(ops, plan)
        if ops.inspect(plan) is not None or ops.read_owner(plan) is not None:
            fail("probe cleanup left an owned object or ownership record")
        if plan.target_wwn and Path("/sys/kernel/config/target/loopback", plan.target_wwn).exists():
            fail("probe cleanup left the owned configfs target")
        if any(loopback_hcls().values()):
            fail("probe cleanup left local SCSI devices")
        if targetcli_session_count() != sessions_before:
            fail("iSCSI session count changed during the local loopback probe")
        require_zero_network_sessions_for_fixture(library_id)
        if not module_was_loaded and not loopback_targets(loopback_root) and not any(loopback_hcls().values()):
            subprocess.run(["modprobe", "-r", "tcm_loop"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10)


def main() -> int:
    parser = argparse.ArgumentParser(description="Isolated LIO tcm_loop mechanism probe; performs INQUIRY only")
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--self-test", action="store_true", help="exercise ownership, rollback and idempotency with a fake runner")
    group.add_argument("--run", metavar="LIBRARY_ID", help="probe only a codex053probe-* fixture library on the Holo Linux host")
    args = parser.parse_args()
    if args.self_test:
        run_self_test()
    else:
        run_real_probe(args.run)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except ProbeError as exc:
        print(f"FAIL: {exc}", file=sys.stderr)
        raise SystemExit(1)
PY
