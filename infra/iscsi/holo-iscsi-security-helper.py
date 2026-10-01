#!/usr/bin/env python3
"""Narrow root helper for Holo-managed iSCSI CHAP and ACL operations."""

import ipaddress
import json
import os
import re
import secrets
import sqlite3
import sys

MAX_INPUT = 1 << 20
IQN_RE = re.compile(r"^iqn\.[0-9]{4}-[0-9]{2}\.[A-Za-z0-9.-]+:[A-Za-z0-9._:-]+$")
ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$")
NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$")
TARGETCLI_SAVE_CONFIG_PATH = "/etc/target/saveconfig.json"
HOLO_ENV_PATH = "/etc/holo/holo.env"
DEFAULT_METADATA_PATH = "/var/lib/holo/holo.db"
DEFAULT_SECRET_KEY_PATH = "/etc/holo/iscsi-secrets.key"


class InvalidRequest(ValueError):
    pass


def _object_no_duplicates(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise InvalidRequest("duplicate_key")
        result[key] = value
    return result


def _reject_constant(_value):
    raise InvalidRequest("invalid_number")


def _keys(value, required, optional=()):
    if not isinstance(value, dict) or not set(required).issubset(value) or set(value) - set(required) - set(optional):
        raise InvalidRequest("invalid_schema")


def _iqn(value):
    if not isinstance(value, str) or not IQN_RE.fullmatch(value) or len(value) > 223:
        raise InvalidRequest("invalid_target")


def _target_iqn(value):
    _iqn(value)
    if not value.startswith("iqn.2026-04.cloud.backupnext.holo:"):
        raise InvalidRequest("invalid_target")


def _secret(value):
    if not isinstance(value, str) or not 12 <= len(value.encode("ascii", "ignore")) <= 255:
        raise InvalidRequest("invalid_credential")
    _username(value)


def _username(value):
    if not isinstance(value, str) or not 1 <= len(value.encode("ascii", "ignore")) <= 255 or not value.isascii():
        raise InvalidRequest("invalid_credential")
    if any(ord(ch) < 0x21 or ord(ch) > 0x7e for ch in value) or value != value.strip() or value[:4].upper() == "NULL":
        raise InvalidRequest("invalid_credential")


def validate_request(request):
    if not isinstance(request, dict) or request.get("version") != 1 or type(request.get("version")) is not int:
        raise InvalidRequest("invalid_envelope")
    operation = request.get("operation")
    if operation in ("capabilities", "vault-provision", "startup-check"):
        _keys(request, ("version", "operation"))
        return
    if operation in ("inspect-target", "delete-owned-target"):
        _keys(request, ("version", "operation", "targetIQN"))
        _target_iqn(request["targetIQN"])
        return
    if operation == "create-target-protected":
        _keys(request, ("version", "operation", "targetIQN", "backstoreName", "backstoreType", "endpoint", "auth", "initiators"))
        _target_iqn(request["targetIQN"])
        if not isinstance(request["backstoreName"], str) or not NAME_RE.fullmatch(request["backstoreName"]):
            raise InvalidRequest("invalid_backstore")
        if not request["backstoreName"].startswith("holo_") or request["backstoreType"] not in ("fileio", "user:holo"):
            raise InvalidRequest("invalid_backstore")
        endpoint = request["endpoint"]
        _keys(endpoint, ("address", "port"))
        _endpoint(endpoint["address"], endpoint["port"])
        if endpoint["port"] != _configured_target_port():
            raise InvalidRequest("invalid_endpoint")
        _validate_auth(request["auth"], request["initiators"])
        return
    raise InvalidRequest("unsupported_operation")


def _endpoint(address, port):
    try:
        parsed = ipaddress.ip_address(address)
    except (TypeError, ValueError):
        raise InvalidRequest("invalid_endpoint")
    if parsed.version != 4 or parsed.is_unspecified or parsed.is_loopback or parsed.is_multicast or parsed.is_link_local:
        raise InvalidRequest("invalid_endpoint")
    if type(port) is not int or not 1024 <= port <= 65535:
        raise InvalidRequest("invalid_endpoint")


def _configured_target_port():
    path = HOLO_ENV_PATH
    try:
        _root_file(path, 0o640)
    except FileNotFoundError:
        return 3260
    with open(path, "r", encoding="utf-8") as env_file:
        for line in env_file:
            if line.startswith("HOLO_TARGET_PORTAL_PORT="):
                value = line.split("=", 1)[1].strip()
                if value.isdecimal() and 1024 <= int(value) <= 65535:
                    return int(value)
                raise InvalidRequest("invalid_target_port")
    return 3260


def _validate_auth(auth, initiators):
    if not isinstance(auth, dict) or auth.get("mode") not in ("none", "chap", "mutual_chap"):
        raise InvalidRequest("invalid_auth")
    restrict_initiators = auth.get("restrictInitiators", False)
    if type(restrict_initiators) is not bool:
        raise InvalidRequest("invalid_auth")
    mode = auth["mode"]
    if mode == "none":
        _keys(auth, ("mode",), ("restrictInitiators",))
    elif mode == "chap":
        _keys(auth, ("mode", "username", "secret"))
        _username(auth["username"])
        _secret(auth["secret"])
    else:
        _keys(auth, ("mode", "username", "secret", "mutualUsername", "mutualSecret"))
        _username(auth["username"])
        _secret(auth["secret"])
        _username(auth["mutualUsername"])
        _secret(auth["mutualSecret"])
        if auth["secret"] == auth["mutualSecret"]:
            raise InvalidRequest("invalid_auth")
    if not isinstance(initiators, list) or len(initiators) > 256:
        raise InvalidRequest("invalid_initiators")
    normalized = set()
    for initiator in initiators:
        _iqn(initiator)
        if initiator != initiator.strip() or initiator.lower() in normalized:
            raise InvalidRequest("invalid_initiators")
        normalized.add(initiator.lower())
    if mode in ("chap", "mutual_chap") and not initiators:
        raise InvalidRequest("invalid_initiators")
    if mode == "none" and not initiators and not restrict_initiators:
        raise InvalidRequest("open_policy_not_protected")


def process_bytes(raw):
    if not isinstance(raw, bytes) or len(raw) > MAX_INPUT:
        return {"ok": False, "code": "request_too_large"}
    try:
        request = json.loads(raw.decode("utf-8"), object_pairs_hook=_object_no_duplicates, parse_constant=_reject_constant)
        validate_request(request)
    except (UnicodeDecodeError, json.JSONDecodeError, InvalidRequest, RecursionError):
        return {"ok": False, "code": "invalid_request"}
    if request["operation"] == "capabilities":
        return {"ok": True, "code": "ok", "revision": "holo-iscsi-security-v1"}
    if request["operation"] == "vault-provision":
        try:
            _provision_empty_vault_key()
            return {"ok": True, "code": "ok", "revision": "holo-iscsi-security-v1"}
        except Exception:
            return {"ok": False, "code": "operation_failed"}
    if request["operation"] == "startup-check":
        try:
            _check_saved_target_config()
            return {"ok": True, "code": "ok", "revision": "holo-iscsi-security-v1", "ready": True}
        except Exception:
            return {"ok": False, "code": "operation_failed"}
    if request["operation"] == "create-target-protected":
        try:
            _create_protected_target(request)
            return {"ok": True, "code": "ok", "revision": "holo-iscsi-security-v1", "ready": True}
        except Exception:
            return {"ok": False, "code": "operation_failed"}
    if request["operation"] == "delete-owned-target":
        try:
            _delete_owned_target(request["targetIQN"])
            return {"ok": True, "code": "ok", "revision": "holo-iscsi-security-v1"}
        except Exception:
            return {"ok": False, "code": "operation_failed"}
    if request["operation"] == "inspect-target":
        try:
            return {"ok": True, "code": "ok", "ready": _target_exists(request["targetIQN"])}
        except Exception:
            return {"ok": False, "code": "operation_failed"}
    return {"ok": False, "code": "unsupported_runtime"}


def _rtslib():
    import rtslib_fb

    return rtslib_fb


def _target_exists(target_iqn):
    rtslib = _rtslib()
    fabric = rtslib.FabricModule("iscsi")
    return any(target.wwn == target_iqn for target in fabric.targets)


def _find_backstore(rtslib, kind, name):
    matches = [item for item in rtslib.RTSRoot().storage_objects if item.name == name and item.plugin == kind]
    if len(matches) != 1:
        raise InvalidRequest("backstore_missing")
    return matches[0]


def _create_protected_target(request):
    rtslib = _rtslib()
    target_iqn = request["targetIQN"]
    fabric = rtslib.FabricModule("iscsi")
    if any(target.wwn == target_iqn for target in fabric.targets):
        raise InvalidRequest("target_exists")
    backstore_kind = request["backstoreType"].split(":")[0]
    storage = _find_backstore(rtslib, backstore_kind, request["backstoreName"])
    target = None
    try:
        target = rtslib.Target(fabric, target_iqn, mode="create")
        tpg = rtslib.TPG(target, tag=1, mode="create")
        tpg.enable = False
        auth = request["auth"]
        restricted = bool(request["initiators"]) or auth.get("restrictInitiators", False)
        tpg.set_attribute("authentication", 1 if auth["mode"] in ("chap", "mutual_chap") else 0)
        tpg.set_attribute("generate_node_acls", 0 if restricted else 1)
        tpg.set_attribute("cache_dynamic_acls", 0 if restricted else 1)
        lun = rtslib.LUN(tpg, lun=0, storage_object=storage)
        for initiator in request["initiators"]:
            acl = rtslib.NodeACL(tpg, node_wwn=initiator, mode="create")
            rtslib.MappedLUN(acl, 0, tpg_lun=0)
            if auth["mode"] in ("chap", "mutual_chap"):
                acl.chap_userid = auth["username"]
                acl.chap_password = auth["secret"]
                acl.chap_mutual_userid = auth.get("mutualUsername", "")
                acl.chap_mutual_password = auth.get("mutualSecret", "")
        endpoint = request["endpoint"]
        rtslib.NetworkPortal(tpg, endpoint["address"], endpoint["port"], mode="create")
        portals = list(tpg.network_portals)
        luns = list(tpg.luns)
        acls = list(tpg.node_acls)
        if tpg.enable is not False or len(portals) != 1 or len(luns) != 1:
            raise InvalidRequest("target_readback_failed")
        if portals[0].ip_address != endpoint["address"] or portals[0].port != endpoint["port"]:
            raise InvalidRequest("target_portal_readback_failed")
        if len(acls) != len(request["initiators"]):
            raise InvalidRequest("target_acl_readback_failed")
        if auth["mode"] in ("chap", "mutual_chap"):
            for acl in acls:
                if acl.chap_userid != auth["username"] or acl.chap_password != auth["secret"]:
                    raise InvalidRequest("target_auth_readback_failed")
                if acl.chap_mutual_userid != auth.get("mutualUsername", "") or acl.chap_mutual_password != auth.get("mutualSecret", ""):
                    raise InvalidRequest("target_auth_readback_failed")
        tpg.enable = True
        if tpg.enable is not True:
            raise InvalidRequest("target_enable_failed")
    except Exception:
        if target is not None:
            try:
                target.delete()
            except Exception:
                pass
        raise


def _delete_owned_target(target_iqn):
    rtslib = _rtslib()
    fabric = rtslib.FabricModule("iscsi")
    matches = [target for target in fabric.targets if target.wwn == target_iqn]
    if not matches:
        return
    if len(matches) != 1:
        raise InvalidRequest("target_ambiguous")
    matches[0].delete()


def _root_file(path, mode=None):
    info = os.lstat(path)
    if not os.path.isfile(path) or os.path.islink(path) or info.st_uid != 0:
        raise InvalidRequest("unsafe_root_file")
    if mode is not None and (info.st_mode & 0o777) != mode:
        raise InvalidRequest("unsafe_root_file")
    return info


def _check_saved_target_config():
    path = TARGETCLI_SAVE_CONFIG_PATH
    try:
        info = _root_file(path)
    except FileNotFoundError:
        return
    if info.st_size > 16 << 20:
        raise InvalidRequest("unsafe_target_saveconfig")
    with open(path, "r", encoding="utf-8") as config_file:
        config = json.load(config_file)
    targets = config.get("targets") if isinstance(config, dict) else None
    if not isinstance(targets, list):
        raise InvalidRequest("invalid_target_saveconfig")
    legacy_port = _configured_target_port()
    for target in targets:
        if not isinstance(target, dict) or target.get("fabric") != "iscsi" or not str(target.get("wwn", "")).startswith("iqn.2026-04.cloud.backupnext.holo:"):
            continue
        tpgs = target.get("tpgs", [])
        if not isinstance(tpgs, list):
            raise InvalidRequest("invalid_target_saveconfig")
        for tpg in tpgs:
            if not isinstance(tpg, dict):
                raise InvalidRequest("invalid_target_saveconfig")
            attributes = tpg.get("attributes", {})
            acls = tpg.get("node_acls", [])
            portals = tpg.get("portals", [])
            if not isinstance(attributes, dict) or not isinstance(acls, list) or not isinstance(portals, list):
                raise InvalidRequest("invalid_target_saveconfig")
            authentication = str(attributes.get("authentication", "0")).lower()
            generate_acls = str(attributes.get("generate_node_acls", "1")).lower()
            cache_dynamic_acls = str(attributes.get("cache_dynamic_acls", "1")).lower()
            if authentication not in ("0", "false", "") or generate_acls in ("0", "false") or cache_dynamic_acls in ("0", "false"):
                raise InvalidRequest("stale_holo_security_target")
            for acl in acls:
                if not isinstance(acl, dict):
                    raise InvalidRequest("invalid_target_saveconfig")
                if any(key.startswith("chap_") and value for key, value in acl.items()):
                    raise InvalidRequest("stale_holo_security_target")
            for portal in portals:
                if not isinstance(portal, dict) or type(portal.get("port")) is not int:
                    raise InvalidRequest("invalid_target_saveconfig")
                if portal["port"] != legacy_port:
                    raise InvalidRequest("stale_holo_security_target")


def _provision_empty_vault_key():
    env_path = HOLO_ENV_PATH
    _root_file(env_path, 0o640)
    config = {}
    with open(env_path, "r", encoding="utf-8") as env_file:
        for line in env_file:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, value = line.split("=", 1)
            if key in ("HOLO_METADATA_DSN", "HOLO_ISCSI_SECRET_KEY"):
                config[key] = value
    database_path = config.get("HOLO_METADATA_DSN", DEFAULT_METADATA_PATH)
    key_path = config.get("HOLO_ISCSI_SECRET_KEY", DEFAULT_SECRET_KEY_PATH)
    if not os.path.isabs(database_path) or not os.path.isabs(key_path):
        raise InvalidRequest("unsafe_vault_path")
    database_info = None
    try:
        database_info = os.lstat(database_path)
    except FileNotFoundError:
        pass
    if database_info is not None and (os.path.islink(database_path) or not os.path.isfile(database_path)):
        raise InvalidRequest("unsafe_catalog")
    if database_info is not None:
        connection = sqlite3.connect("file:" + database_path + "?mode=ro", uri=True, timeout=1)
        try:
            tables = {row[0] for row in connection.execute("SELECT name FROM sqlite_master WHERE type='table'")}
            for table in ("iscsi_chap_credentials",):
                if table in tables and connection.execute("SELECT 1 FROM " + table + " LIMIT 1").fetchone() is not None:
                    raise InvalidRequest("vault_not_empty")
        finally:
            connection.close()
    parent = os.path.dirname(key_path)
    parent_info = os.lstat(parent)
    if os.path.islink(parent) or not os.path.isdir(parent) or parent_info.st_uid != 0 or parent_info.st_mode & 0o022:
        raise InvalidRequest("unsafe_key_directory")
    try:
        existing = os.lstat(key_path)
    except FileNotFoundError:
        existing = None
    if existing is not None:
        group = __import__("grp").getgrnam("holo")
        if os.path.islink(key_path) or not os.path.isfile(key_path) or existing.st_uid != 0 or existing.st_gid != group.gr_gid or existing.st_mode & 0o777 != 0o640 or existing.st_nlink != 1:
            raise InvalidRequest("unsafe_existing_key")
        with open(key_path, "rb") as key_file:
            if len(key_file.read(33)) != 32:
                raise InvalidRequest("invalid_existing_key")
        return
    group = __import__("grp").getgrnam("holo")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    fd = os.open(key_path, flags, 0o600)
    try:
        os.fchown(fd, 0, group.gr_gid)
        os.fchmod(fd, 0o640)
        os.write(fd, secrets.token_bytes(32))
        os.fsync(fd)
    except Exception:
        os.close(fd)
        try:
            os.unlink(key_path)
        except OSError:
            pass
        raise
    os.close(fd)


def main():
    if os.geteuid() != 0 or len(sys.argv) != 1:
        sys.stdout.write('{"ok":false,"code":"not_authorized"}\n')
        return 1
    raw = sys.stdin.buffer.read(MAX_INPUT + 1)
    response = process_bytes(raw)
    sys.stdout.write(json.dumps(response, separators=(",", ":")) + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
