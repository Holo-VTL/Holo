#!/usr/bin/env python3
"""Fail closed unless go version -m matches the release build contract."""

import argparse
import json
from pathlib import Path
import re
import sys


def normalize_go_version(value: str) -> str:
    return value[2:] if value.startswith("go") else value


def verify(text: str, go_version: str, goos: str, goarch: str, cgo: str, module: str) -> dict:
    lines = text.splitlines()
    expected_go = normalize_go_version(go_version)
    if not lines or not re.search(rf"\bgo{re.escape(expected_go)}\s*$", lines[0]):
        raise ValueError("binary was built with a different Go toolchain")
    values = {}
    main_module = ""
    for raw_line in lines[1:]:
        line = raw_line.strip()
        parts = line.split(None, 1)
        if len(parts) != 2:
            continue
        kind, payload = parts
        if kind == "path":
            main_module = payload.strip()
        if kind != "build" or "=" not in payload:
            continue
        key, value = payload.split("=", 1)
        if key in values and values[key] != value:
            raise ValueError(f"binary contains conflicting build setting {key}")
        values[key] = value
    expected = {"GOOS": goos, "GOARCH": goarch, "CGO_ENABLED": cgo}
    for key, value in expected.items():
        if values.get(key) != value:
            raise ValueError(f"binary build setting {key} does not match {value}")
    if not main_module or main_module != module:
        raise ValueError("binary main package identity does not match the control-plane")
    return {"go_version": "go" + expected_go, "main_module": main_module, "build_settings": expected}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--input", required=True)
    parser.add_argument("--go-version", required=True)
    parser.add_argument("--goos", required=True)
    parser.add_argument("--goarch", required=True)
    parser.add_argument("--cgo", required=True)
    parser.add_argument("--module", required=True)
    parser.add_argument("--output")
    args = parser.parse_args()
    try:
        evidence = verify(Path(args.input).read_text(encoding="utf-8"), args.go_version, args.goos, args.goarch, args.cgo, args.module)
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
