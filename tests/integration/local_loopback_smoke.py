#!/usr/bin/env python3
"""Exercise the installed Local Mount API and Linux loopback SCSI path."""

from __future__ import annotations

import hashlib
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request


BASE_URL = os.environ.get("HOLO_LOCAL_MOUNT_BASE_URL", "http://127.0.0.1").rstrip("/")
LIBRARY_ID = os.environ.get("HOLO_LOCAL_MOUNT_TEST_LIBRARY", "codex053lmtest")
DRIVE_ID = f"{LIBRARY_ID}-drv01"
HELPER = os.environ.get("HOLO_LOCAL_LOOPBACK_HELPER", "/opt/holo/bin/holo-local-loopback-helper")


def api(method: str, path: str, payload: dict | None = None):
    data = None if payload is None else json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(
        BASE_URL + path,
        data=data,
        method=method,
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            body = response.read()
            return response.status, json.loads(body) if body else None
    except urllib.error.HTTPError as exc:
        body = exc.read(512)
        raise RuntimeError(f"{method} {path} returned HTTP {exc.code}: {body!r}") from None


def helper(operation: str) -> dict:
    payload = json.dumps({"version": 1, "operation": operation}).encode("utf-8")
    result = subprocess.run([HELPER], input=payload, check=True, capture_output=True, timeout=10)
    return json.loads(result.stdout)


def session_count() -> int:
    output = subprocess.check_output(
        ["ss", "-Htn", "state", "established", "( sport = :3260 )"], text=True
    )
    return len([line for line in output.splitlines() if line.strip()])


def wait_for(predicate, timeout: int = 60) -> dict:
    current = {}
    for _ in range(timeout):
        _, current = api("GET", "/v1/targets/local-mount")
        if predicate(current):
            return current
        if current.get("state") == "failed":
            raise RuntimeError("mount failed: " + json.dumps(current, ensure_ascii=False))
        time.sleep(1)
    raise RuntimeError("timed out waiting for Local Mount: " + json.dumps(current, ensure_ascii=False))


def inspect_devices(status: dict) -> None:
    expected_pdt = {f"changer:{LIBRARY_ID}": 0x08, f"drive:{DRIVE_ID}": 0x01}
    serial_digests = {}
    devices = status.get("devices") or []
    for device in devices:
        key = device.get("deviceKey", "")
        if device.get("state") != "connected":
            raise RuntimeError(f"device is not connected: {key} ({device.get('reasonCode')})")
        paths = [path for path in device.get("observedPaths", []) if path.startswith("/dev/sg")]
        if not paths:
            raise RuntimeError(f"no generic SCSI path was observed for {key}")
        sg_path = paths[0]
        inquiry = subprocess.run(["sg_inq", "--raw", sg_path], check=True, capture_output=True, timeout=10).stdout
        if not inquiry:
            raise RuntimeError(f"empty standard INQUIRY response for {key}")
        pdt = inquiry[0] & 0x1F
        if key in expected_pdt and pdt != expected_pdt[key]:
            raise RuntimeError(f"wrong PDT for {key}: got 0x{pdt:02x}, expected 0x{expected_pdt[key]:02x}")
        for page in (0x80, 0x83):
            raw = subprocess.run(
                ["sg_inq", "--raw", f"--page=0x{page:02x}", sg_path],
                check=True,
                capture_output=True,
                timeout=10,
            ).stdout
            if len(raw) < 4 or raw[1] != page or int.from_bytes(raw[2:4], "big") == 0:
                raise RuntimeError(f"VPD page 0x{page:02x} is missing for {key}")
            if page == 0x80:
                serial_digests[key] = hashlib.sha256(raw).hexdigest()
        print(f"INQUIRY {key} PDT=0x{pdt:02x} sg={sg_path} VPD80_SHA256={serial_digests.get(key)}")

    for key in expected_pdt:
        if key not in serial_digests:
            raise RuntimeError(f"fixture device is absent from the connected inventory: {key}")
    if len(set(serial_digests.values())) != len(serial_digests):
        raise RuntimeError("duplicate VPD page 0x80 serials were observed")


def main() -> None:
    if os.geteuid() != 0:
        raise SystemExit("run this smoke test as root on the Holo host")
    if not LIBRARY_ID.isascii() or not LIBRARY_ID.replace("-", "").isalnum() or len(LIBRARY_ID) > 48:
        raise SystemExit("fixture library ID must be lowercase ASCII letters, digits, and hyphens")
    for command in ("curl", "sg_inq", "lsscsi", "ss"):
        if subprocess.run(["sh", "-c", f"command -v {command} >/dev/null"], check=False).returncode:
            raise SystemExit(f"required command not found: {command}")
    if not os.path.isfile(HELPER) or not os.access(HELPER, os.X_OK):
        raise SystemExit(f"local loopback helper is missing: {HELPER}")

    _, initial = api("GET", "/v1/targets/local-mount")
    if initial.get("enabled") or initial.get("residualDeviceCount", 0):
        raise SystemExit("start with Local Mount disabled and no residuals")
    if helper("list").get("result") != []:
        raise SystemExit("start with no owned loopback mappings")
    if not helper("probe").get("result", {}).get("available"):
        raise SystemExit("loopback capability probe failed")

    _, libraries = api("GET", "/v1/libraries")
    _, drives = api("GET", "/v1/drives")
    _, publications = api("GET", "/v1/targets/publications")
    original_publication_ids = sorted(row["publicationId"] for row in publications)
    original_sessions = session_count()
    expected_devices = len(libraries) + len(drives) + 2
    fixture_created = False
    mount_enabled = False
    failure: Exception | None = None

    try:
        try:
            api("GET", "/v1/libraries/" + LIBRARY_ID)
            raise RuntimeError(f"fixture library already exists: {LIBRARY_ID}")
        except RuntimeError as exc:
            if "returned HTTP 404" not in str(exc):
                raise

        status, _ = api(
            "POST",
            "/v1/libraries",
            {
                "libraryId": LIBRARY_ID,
                "name": "Local Mount Smoke",
                "vendor": "IBM",
                "libraryType": "03584L32",
                "driveType": "ULT3580-TD6",
                "driveCount": 1,
                "slotCount": 8,
            },
        )
        if status != 201:
            raise RuntimeError(f"fixture library create returned HTTP {status}")
        fixture_created = True
        status, drive = api("POST", "/v1/drives", {"driveId": DRIVE_ID, "libraryId": LIBRARY_ID, "slot": 1})
        if status != 201 or drive.get("mountedCartridgeId"):
            raise RuntimeError(f"fixture drive was not created empty: {drive}")
        _, publications_with_fixture = api("GET", "/v1/targets/publications")
        if sorted(row["publicationId"] for row in publications_with_fixture) != original_publication_ids:
            raise RuntimeError("fixture without cartridges unexpectedly created a network publication")

        api("POST", "/v1/targets/local-mount", {"enabled": True, "actor": "local-loopback-smoke"})
        mount_enabled = True
        connected = wait_for(
            lambda value: value.get("state") == "connected"
            and value.get("desiredDeviceCount") == expected_devices
            and value.get("connectedDeviceCount") == expected_devices
        )
        inspect_devices(connected)

        _, publications_after_mount = api("GET", "/v1/targets/publications")
        if sorted(row["publicationId"] for row in publications_after_mount) != original_publication_ids:
            raise RuntimeError("network publication inventory changed during local mount")
        if session_count() != original_sessions:
            raise RuntimeError("network iSCSI session count changed during local mount")

    except Exception as exc:
        failure = exc
    finally:
        if mount_enabled:
            try:
                api("POST", "/v1/targets/local-mount", {"enabled": False, "actor": "local-loopback-smoke-cleanup"})
                wait_for(lambda value: value.get("state") == "disabled" and value.get("residualDeviceCount") == 0)
                mount_enabled = False
            except Exception as exc:
                failure = failure or RuntimeError(f"could not confirm clean Local Mount disable: {exc}")
        if fixture_created:
            try:
                if helper("list").get("result") != []:
                    raise RuntimeError("owned loopback objects remain; keeping fixture for recovery")
                api("DELETE", "/v1/libraries/" + LIBRARY_ID)
                fixture_created = False
            except Exception as exc:
                failure = failure or RuntimeError(f"could not clean the temporary library: {exc}")

    if failure:
        raise SystemExit(f"FAIL: {failure}")
    _, final_publications = api("GET", "/v1/targets/publications")
    if sorted(row["publicationId"] for row in final_publications) != original_publication_ids:
        raise SystemExit("network publication inventory changed after cleanup")
    if helper("list").get("result") != []:
        raise SystemExit("owned loopback mappings remain after cleanup")
    if session_count() != original_sessions:
        raise SystemExit("network iSCSI session count changed after cleanup")
    print(
        f"PASS: {expected_devices}/{expected_devices} local devices including an empty no-Pool changer/drive; "
        "PDT and VPD 0x80/0x83 verified; local mappings removed; network targets and sessions unchanged"
    )


if __name__ == "__main__":
    main()
