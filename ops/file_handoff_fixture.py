#!/usr/bin/env python3
"""Disposable GitHub-runner setup only; the Go test runs as an ordinary UID."""
import argparse
import json
import os
from pathlib import Path
import pwd
import shutil
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--test-binary", required=True)
    args = parser.parse_args()
    if os.geteuid() != 0 or os.environ.get("GITHUB_ACTIONS") != "true":
        raise SystemExit("requires explicitly authorized disposable GitHub runner setup")
    gateway, model = "cc-file-fixture-gateway", "cc-file-fixture-model"
    created = []
    try:
        for name in (model, gateway):
            try:
                pwd.getpwnam(name)
            except KeyError:
                subprocess.run(["useradd", "--system", "--no-create-home", "--user-group", name], check=True)
                created.append(name)
            else:
                raise RuntimeError("fixture account already exists")
        gw, agent = pwd.getpwnam(gateway), pwd.getpwnam(model)
        subprocess.run(["usermod", "-aG", model, gateway], check=True)
        with tempfile.TemporaryDirectory(prefix="cc-file-identities-") as temporary:
            root = Path(temporary)
            root.chmod(0o755)
            binary = root / "handoff.test"
            shutil.copyfile(args.test_binary, binary)
            binary.chmod(0o555)
            workspace = root / "gateway"
            workspace.mkdir(mode=0o750)
            os.chown(workspace, gw.pw_uid, agent.pw_gid)
            for name in ("works", "tmp"):
                directory = workspace / name
                directory.mkdir(mode=0o750)
                os.chown(directory, gw.pw_uid, agent.pw_gid)
            legacy = workspace / "legacy-private"
            legacy.mkdir(mode=0o700)
            (legacy / "result.docx").write_text("synthetic permission fixture", encoding="utf-8")
            os.chown(legacy, agent.pw_uid, agent.pw_gid)
            settings = root / "fixture.json"
            settings.write_text(json.dumps({"Root": str(workspace), "GatewayUID": gw.pw_uid, "ModelUID": agent.pw_uid}), encoding="utf-8")
            settings.chmod(0o444)
            env = {"PATH": "/usr/bin:/bin", "TMPDIR": str(workspace / "tmp"),
                   "CC_FILE_HANDOFF_FIXTURE": str(settings)}
            result = subprocess.run(["setpriv", "--reuid", str(gw.pw_uid), "--regid", str(gw.pw_gid),
                "--init-groups", "--bounding-set=-all", "--inh-caps=-all", "--ambient-caps=-all", "--",
                str(binary), "-test.run=^TestWorkHandoffOrdinaryIdentities$", "-test.v", "-test.timeout=90s"], env=env, timeout=100)
            return result.returncode
    finally:
        for name in reversed(created):
            subprocess.run(["userdel", name], check=True)


if __name__ == "__main__":
    raise SystemExit(main())
