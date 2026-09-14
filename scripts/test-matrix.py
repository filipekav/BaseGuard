#!/usr/bin/env python3
"""Run every pinned server against the native application, one at a time."""
import json
import os
import platform
import subprocess
from pathlib import Path

root = Path(__file__).resolve().parents[1]
lock = json.loads((root / "clients.lock.json").read_text())
arch = "arm64" if platform.machine() in ("aarch64", "arm64") else "amd64"
certs = root / ".cache" / "test-certs"
certs.mkdir(parents=True, exist_ok=True)
def openssl(*args):
    subprocess.run(["openssl", *args], cwd=certs, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2", "-subj", "/CN=BaseGuard integration CA", "-keyout", "ca.key", "-out", "ca.crt")
openssl("req", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=db", "-keyout", "server.key", "-out", "server.csr")
(certs / "extensions.cnf").write_text("subjectAltName=DNS:db\nextendedKeyUsage=serverAuth\n")
openssl("x509", "-req", "-in", "server.csr", "-CA", "ca.crt", "-CAkey", "ca.key", "-CAcreateserial", "-days", "2", "-extfile", "extensions.cnf", "-out", "server.crt")
openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2", "-subj", "/CN=Wrong CA", "-keyout", "wrong.key", "-out", "wrong.crt")
compose = ["docker", "compose", "-f", "compose.test.yaml"]
failures = []
for row in lock["images"]:
    engine, tag = row["image"].split(":")
    series = tag.split(".")[0] if engine == "postgres" else ".".join(tag.split(".")[:2])
    env = dict(os.environ, BASEGUARD_TEST_IMAGE=row["image"] + "@" + row["digest"],
               BASEGUARD_TEST_ENGINE=engine, BASEGUARD_TEST_SERIES=series,
               BASEGUARD_TEST_PLATFORM="linux/" + (arch if arch in row["architectures"] else "amd64"))
    for no_tls in ("0", "1"):
        env["BASEGUARD_TEST_NO_TLS"] = no_tls
        print(f"::group::{row['image']} / native client {arch} / no_tls={no_tls}", flush=True)
        try:
            result = subprocess.run(compose + ["up", "--build", "--abort-on-container-exit", "--exit-code-from", "tests"],
                                    cwd=root, env=env, check=False)
            if result.returncode:
                failures.append(f"{row['image']} no_tls={no_tls}")
        finally:
            subprocess.run(compose + ["down", "-v"], cwd=root, env=env, check=True)
            print("::endgroup::", flush=True)
if failures:
    raise SystemExit("Compatibility tests failed: " + ", ".join(failures))
