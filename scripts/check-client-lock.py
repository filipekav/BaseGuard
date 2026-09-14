#!/usr/bin/env python3
"""Offline guard: native profiles and Docker stages must agree with the lock."""
import json
import re
from pathlib import Path

root = Path(__file__).resolve().parents[1]
images = json.loads((root / "clients.lock.json").read_text())["images"]
dockerfile = (root / "Dockerfile").read_text()
refs = set(re.findall(r"^FROM (\S+@sha256:[a-f0-9]{64}) AS ", dockerfile, re.M))
expected = set()
for row in images:
    assert re.fullmatch(r"sha256:[a-f0-9]{64}", row["digest"]), row
    if row["image"].startswith("mysql:5.7"):
        assert "amd64" in row["architectures"], row
        continue  # test-only servers; native MySQL 8.0 clients perform backups
    assert {"amd64", "arm64"} <= set(row["architectures"]), row
    expected.add(row["image"] + "@" + row["digest"])
assert refs == expected, f"Dockerfile/client lock mismatch: {refs ^ expected}"
assert len(expected) == 14, "Expected 7 PostgreSQL + 2 MySQL + 5 MariaDB clients"
print("Client image lock and native architectures verified")
