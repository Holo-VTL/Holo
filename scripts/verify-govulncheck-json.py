#!/usr/bin/env python3
"""Validate govulncheck's pinned streaming JSON contract and emit scan evidence."""

import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import sys
from urllib.parse import urlparse


MESSAGE_KEYS = {"config", "progress", "SBOM", "osv", "finding"}
CONFIG_KEYS = {
    "protocol_version",
    "scanner_name",
    "scanner_version",
    "db",
    "db_last_modified",
    "go_version",
    "scan_level",
    "scan_mode",
}


def normalize_go_version(value: str) -> str:
    return value[2:] if value.startswith("go") else value


def _fail(message: str) -> None:
    raise ValueError(message)


def verify(payload: str, mode: str, go_version: str, scanner_version: str, goos: str, goarch: str) -> dict:
    config = None
    sbom_go_version = None
    findings = []
    osv_entries = {}
    messages = 0
    decoder = json.JSONDecoder()
    offset = 0
    while offset < len(payload):
        while offset < len(payload) and payload[offset].isspace():
            offset += 1
        if offset >= len(payload):
            break
        try:
            message, offset = decoder.raw_decode(payload, offset)
        except json.JSONDecodeError as exc:
            _fail(f"offset {exc.pos}: malformed JSON ({exc.msg})")
        if not isinstance(message, dict) or len(message) != 1:
            _fail("expected one typed govulncheck message")
        key, value = next(iter(message.items()))
        if key not in MESSAGE_KEYS or not isinstance(value, dict):
            _fail("unknown or malformed message type")
        messages += 1
        if key == "config":
            if config is not None or messages != 1:
                _fail("govulncheck config must appear once, as the first message")
            unknown = set(value) - CONFIG_KEYS
            if unknown:
                _fail("govulncheck config contains unknown fields")
            required = ("protocol_version", "scanner_name", "scanner_version", "db", "db_last_modified", "scan_mode")
            if any(not isinstance(value.get(field), str) or not value[field] for field in required):
                _fail("govulncheck config is missing required scan metadata")
            if value["scanner_name"] != "govulncheck" or scanner_version not in value["scanner_version"]:
                _fail("govulncheck scanner identity does not match the pinned version")
            if value["scan_mode"] != mode:
                _fail("govulncheck scan mode does not match the requested mode")
            if "go_version" in value and normalize_go_version(value["go_version"]) != normalize_go_version(go_version):
                _fail("govulncheck analyzed a different Go toolchain")
            if mode == "source" and "go_version" not in value:
                _fail("source scan config has no Go toolchain version")
            database = urlparse(value["db"] if "://" in value["db"] else "https://" + value["db"])
            if database.hostname != "vuln.go.dev":
                _fail("govulncheck did not use the canonical Go vulnerability database")
            try:
                timestamp = value["db_last_modified"]
                timestamp = timestamp[:-1] + "+0000" if timestamp.endswith("Z") else timestamp
                if len(timestamp) >= 6 and timestamp[-3] == ":":
                    timestamp = timestamp[:-3] + timestamp[-2:]
                try:
                    modified = datetime.strptime(timestamp, "%Y-%m-%dT%H:%M:%S.%f%z")
                except ValueError:
                    modified = datetime.strptime(timestamp, "%Y-%m-%dT%H:%M:%S%z")
            except ValueError:
                _fail("govulncheck did not report a valid database timestamp")
            if modified.tzinfo is None or modified > datetime.now(timezone.utc):
                _fail("govulncheck database timestamp is invalid or in the future")
            config = value
        elif key == "SBOM":
            value_go_version = value.get("go_version")
            if value_go_version is not None:
                if not isinstance(value_go_version, str) or not value_go_version:
                    _fail("govulncheck SBOM Go version has an invalid shape")
                sbom_go_version = value_go_version
        elif key == "finding":
            osv = value.get("osv")
            trace = value.get("trace", [])
            if not isinstance(osv, str) or not osv or not isinstance(trace, list):
                _fail("govulncheck finding shape is invalid")
            called_symbols = []
            for frame in trace:
                if not isinstance(frame, dict):
                    _fail("govulncheck finding trace is invalid")
                symbol = frame.get("function") or frame.get("Function")
                if isinstance(symbol, str) and symbol:
                    called_symbols.append(symbol)
            findings.append({
                "osv": osv,
                "level": "symbol" if called_symbols else ("package" if trace else "module"),
                "symbols": called_symbols,
            })
        elif key == "osv":
            osv_id = value.get("id")
            if not isinstance(osv_id, str) or not osv_id:
                _fail("govulncheck OSV entry has no identifier")
            database_specific = value.get("database_specific", {})
            if not isinstance(database_specific, dict):
                _fail("govulncheck OSV metadata has an invalid shape")
            severity = database_specific.get("severity")
            if severity is not None and not isinstance(severity, str):
                _fail("govulncheck OSV severity has an invalid shape")
            affected_platforms = set()
            affected_symbols = set()
            affected_entries = value.get("affected", [])
            if not isinstance(affected_entries, list):
                _fail("govulncheck OSV affected list has an invalid shape")
            for affected in affected_entries:
                if not isinstance(affected, dict):
                    _fail("govulncheck OSV affected entry has an invalid shape")
                ecosystem = affected.get("ecosystem_specific", {})
                imports = ecosystem.get("imports", []) if isinstance(ecosystem, dict) else []
                if not isinstance(imports, list):
                    _fail("govulncheck OSV import metadata has an invalid shape")
                for imported in imports:
                    if not isinstance(imported, dict):
                        _fail("govulncheck OSV import metadata has an invalid shape")
                    platforms = imported.get("goos", [])
                    symbols = imported.get("symbols", [])
                    if not isinstance(platforms, list) or not all(isinstance(item, str) for item in platforms):
                        _fail("govulncheck OSV platform metadata has an invalid shape")
                    if not isinstance(symbols, list) or not all(isinstance(item, str) for item in symbols):
                        _fail("govulncheck OSV symbol metadata has an invalid shape")
                    affected_platforms.update(platforms)
                    affected_symbols.update(symbols)
            osv_entries[osv_id] = {
                "severity": severity.upper() if isinstance(severity, str) else "unknown",
                "affected_platforms": sorted(affected_platforms),
                "affected_symbols": sorted(affected_symbols),
            }
    if messages == 0 or config is None:
        _fail("govulncheck produced no complete scan metadata")
    analyzed_go_version = config.get("go_version") or sbom_go_version
    if analyzed_go_version is None or normalize_go_version(analyzed_go_version) != normalize_go_version(go_version):
        _fail("govulncheck analyzed a different or unknown Go toolchain")
    for finding in findings:
        advisory = osv_entries.get(finding["osv"], {})
        finding["severity"] = advisory.get("severity", "unknown")
        finding["affected_platforms"] = advisory.get("affected_platforms", [])
        finding["affected_symbols"] = advisory.get("affected_symbols", [])
        finding["target_relevant"] = not finding["affected_platforms"] or goos in finding["affected_platforms"]
    return {
        "scan_mode": mode,
        "target": {"goos": goos, "goarch": goarch},
        "scanner_version": config["scanner_version"],
        "go_version": analyzed_go_version,
        "database": config["db"],
        "database_last_modified": config["db_last_modified"],
        "findings": findings,
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--input", required=True)
    parser.add_argument("--mode", choices=("source", "binary"), required=True)
    parser.add_argument("--go-version", required=True)
    parser.add_argument("--scanner-version", required=True)
    parser.add_argument("--goos", required=True)
    parser.add_argument("--goarch", required=True)
    parser.add_argument("--output")
    args = parser.parse_args()
    try:
        evidence = verify(Path(args.input).read_text(encoding="utf-8"), args.mode, args.go_version, args.scanner_version, args.goos, args.goarch)
        if any(item["level"] == "symbol" and item["target_relevant"] for item in evidence["findings"]):
            _fail("govulncheck found symbol-level findings that require disposition")
        if any(item["severity"] in {"HIGH", "CRITICAL"} and item["target_relevant"] for item in evidence["findings"]):
            _fail("govulncheck found a high-severity advisory that requires disposition")
        if any(item["severity"] == "unknown" and item["target_relevant"] for item in evidence["findings"]):
            _fail("govulncheck found a target-relevant advisory with unclassified severity")
        serialized = json.dumps(evidence, sort_keys=True, separators=(",", ":")) + "\n"
        if args.output:
            Path(args.output).write_text(serialized, encoding="utf-8")
        else:
            sys.stdout.write(serialized)
        return 0
    except (OSError, UnicodeError, ValueError) as exc:
        print(f"[release-security] {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
