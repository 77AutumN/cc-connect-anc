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
    sudoers = Path('/etc/sudoers.d/cc-file-handoff-fixture')
    userns_policy = Path('/proc/sys/kernel/apparmor_restrict_unprivileged_userns')
    previous_policy = None
    sudoers_created = False
    try:
        if sudoers.exists():
            raise RuntimeError('fixture sudo rule already exists')
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
            bundle = workspace / 'launcher'
            bundle.mkdir(mode=0o750)
            os.chown(bundle, gw.pw_uid, agent.pw_gid)
            for name in ('work_sandbox.py', 'file_tool.py', 'file_handoff_probe.py'):
                destination = bundle / name
                shutil.copyfile(Path(__file__).with_name(name), destination)
                os.chown(destination, gw.pw_uid, agent.pw_gid)
                destination.chmod(0o550)
            command = str(Path('/usr/bin/python3').resolve(strict=True))
            config = bundle / 'sandbox.json'
            config.write_text(json.dumps({'enabled':True, 'work_base_dir':str(workspace/'works'),
                'action_tools_socket':str(workspace/'tools.sock'), 'command':command}), encoding='utf-8')
            os.chown(config, gw.pw_uid, agent.pw_gid)
            config.chmod(0o440)
            # CI-only, one target UID and fixed launcher/config. Never grants
            # the gateway a root command or any effective DAC capability.
            with sudoers.open('x', encoding='utf-8') as stream:
                sudoers_created = True
                stream.write(f'Defaults:{gateway} env_keep += "CC_FILE_ACTION_TOKEN"\n'
                    f'{gateway} ALL=({model}) NOPASSWD: /usr/bin/python3 -I -B {bundle}/work_sandbox.py --config {config} --work-root {workspace}/works/* -- {command} -c *\n')
            sudoers.chmod(0o440)
            subprocess.run(['visudo', '-cf', str(sudoers)], check=True)
            # Ubuntu hosted runners restrict ordinary user namespaces through
            # AppArmor. Explicit disposable setup, restored below; this is not
            # evidence about an unchanged VPS kernel/security policy.
            if userns_policy.exists():
                previous_policy = userns_policy.read_text()
                userns_policy.write_text('0\n')
            legacy = workspace / "legacy-private"
            legacy.mkdir(mode=0o700)
            (legacy / "result.docx").write_text("synthetic permission fixture", encoding="utf-8")
            os.chown(legacy, agent.pw_uid, agent.pw_gid)
            settings = root / "fixture.json"
            settings.write_text(json.dumps({"Root": str(workspace), "GatewayUID": gw.pw_uid, "ModelUID": agent.pw_uid,
                'Model':model, 'Command':command, 'Launcher':str(bundle/'work_sandbox.py'),
                'Config':str(config), 'Probe':str(bundle/'file_handoff_probe.py')}), encoding="utf-8")
            settings.chmod(0o444)
            env = {"PATH": "/usr/bin:/bin", "TMPDIR": str(workspace / "tmp"),
                   "CC_FILE_HANDOFF_FIXTURE": str(settings)}
            result = subprocess.run(["setpriv", "--reuid", str(gw.pw_uid), "--regid", str(gw.pw_gid),
                "--init-groups", "--inh-caps=-all", "--ambient-caps=-all", "--",
                str(binary), "-test.run=^TestWorkHandoffOrdinaryIdentities$", "-test.v", "-test.timeout=90s"], env=env, timeout=100)
            return result.returncode
    finally:
        if previous_policy is not None:
            userns_policy.write_text(previous_policy)
        if sudoers_created:
            sudoers.unlink()
        for name in reversed(created):
            subprocess.run(["userdel", name], check=True)


if __name__ == "__main__":
    raise SystemExit(main())
