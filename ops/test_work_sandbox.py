#!/usr/bin/env python3
"""Synthetic-only boundary tests; never execute a model or external request."""

import argparse
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest.mock import patch

import work_sandbox


REQUIRE_ISOLATION = "--require-isolation" in sys.argv
if REQUIRE_ISOLATION:
    sys.argv.remove("--require-isolation")
LAUNCHER = Path(__file__).with_name("work_sandbox.py").resolve()


class AdmissionTests(unittest.TestCase):
    def test_unsupported_platform_never_executes_command(self):
        args = argparse.Namespace(config="unused", work_root="unused", command=["unused"])
        with patch.object(work_sandbox.sys, "platform", "win32"), \
                patch.object(work_sandbox.subprocess, "Popen") as start:
            with self.assertRaises(work_sandbox.Refused):
                work_sandbox.launch(args)
            start.assert_not_called()

    def test_relative_and_parent_paths_refused(self):
        for value in ("", "relative", "../work", "/safe/../other", None):
            with self.subTest(value=value), self.assertRaises(work_sandbox.Refused):
                work_sandbox.absolute(value)


class LinuxBoundaryTests(unittest.TestCase):
    def setUp(self):
        if sys.platform != "linux" or os.geteuid() != 0:
            if REQUIRE_ISOLATION:
                self.fail("isolated Linux fixture requires existing sudo; no test was run")
            self.skipTest("Linux fixture is run with existing sudo in disposable CI only")
        self.tmp = tempfile.TemporaryDirectory(prefix="cc-sandbox-fixture-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        bundle = self.root / "launcher"
        bundle.mkdir()
        self.launcher = bundle / "work_sandbox.py"
        for name in ("work_sandbox.py", "file_tool.py"):
            copied = bundle / name
            copied.write_bytes(LAUNCHER.with_name(name).read_bytes())
            copied.chmod(0o555)
        self.base = self.root / "works"
        self.work = self.base / "current"
        (self.work / "inputs").mkdir(parents=True)
        (self.work / "outputs").mkdir()
        (self.work / "inputs" / "input.txt").write_text("synthetic-input", encoding="utf-8")
        self.config = self.root / "sandbox.json"
        self.command = str(Path("/usr/bin/python3").resolve(strict=True))
        self.socket_path = self.root / "tools.sock"
        self.listener = socket.socket(socket.AF_UNIX)
        self.listener.bind(str(self.socket_path))
        self.addCleanup(self.listener.close)
        self.settings = {"enabled": True, "work_base_dir": str(self.base),
                         "action_tools_socket": str(self.socket_path), "command": self.command}
        self.save_config()

    def save_config(self):
        self.config.write_text(json.dumps(self.settings), encoding="utf-8")
        self.config.chmod(0o600)

    def invoke(self, *command):
        env = {"PATH": "/usr/bin:/bin", "CC_FILE_ACTION_TOKEN": "synthetic-file-capability",
               "MYANC_CRM_ACTION_TOKEN": "synthetic-file-capability",
               "HOST_ONLY_SENTINEL": "synthetic-host-environment", "PYTHONPATH": "/host-only"}
        return subprocess.run([sys.executable, "-I", "-B", str(self.launcher), "--config", str(self.config),
            "--work-root", str(self.work), "--", *command], input="", capture_output=True,
            text=True, timeout=30, env=env)

    def test_disabled_missing_unsafe_configuration_never_executes(self):
        marker = self.work / "outputs" / "must-not-exist"
        command = (self.command, "-c", f"open({str(marker)!r}, 'w').write('bad')")
        for field, value in (("enabled", False), ("work_base_dir", str(self.root)),
                             ("command", "/usr/bin/false"), ("action_tools_socket", str(self.config))):
            original = self.settings[field]
            self.settings[field] = value
            self.save_config()
            self.assertNotEqual(self.invoke(*command).returncode, 0)
            self.assertFalse(marker.exists())
            self.settings[field] = original
        self.save_config()
        self.config.chmod(0o666)
        self.assertNotEqual(self.invoke(*command).returncode, 0)
        self.config.unlink()
        self.assertNotEqual(self.invoke(*command).returncode, 0)
        self.assertFalse(marker.exists())

    def test_symlink_and_hardlink_inputs_refused(self):
        secret = self.root / "host-auth"
        secret.write_text("synthetic-host-auth", encoding="utf-8")
        linked = self.work / "inputs" / "linked"
        linked.symlink_to(secret)
        self.assertNotEqual(self.invoke(self.command, "-c", "raise SystemExit(0)").returncode, 0)
        linked.unlink()
        os.link(secret, linked)
        self.assertNotEqual(self.invoke(self.command, "-c", "raise SystemExit(0)").returncode, 0)

    def test_actual_files_process_environment_network_and_tool_boundary(self):
        other = self.base / "other"
        other.mkdir()
        host_paths = [self.config, other, self.root / "host-auth", self.root / "host-policy",
                      self.root / "host-database", self.root / "admin.sock"]
        for path in host_paths[2:-1]:
            path.write_text("synthetic-host-only", encoding="utf-8")
        admin = socket.socket(socket.AF_UNIX)
        admin.bind(str(host_paths[-1]))
        self.addCleanup(admin.close)
        host_tcp = socket.socket()
        host_tcp.bind(("127.0.0.1", 0))
        host_tcp.listen()
        self.addCleanup(host_tcp.close)
        sibling = subprocess.Popen([self.command, "-c", "import time; time.sleep(40)"],
                                   env={"HOST_SIBLING_SENTINEL": "synthetic-sibling"})
        self.addCleanup(sibling.wait)
        self.addCleanup(sibling.kill)
        self.listener.listen()
        self.listener.settimeout(25)
        requests = []

        def serve_tool():
            try:
                with self.listener.accept()[0] as connection:
                    connection.settimeout(5)
                    request = b""
                    while b"\r\n\r\n" not in request:
                        part = connection.recv(8192)
                        if not part:
                            return
                        request += part
                    headers, body = request.split(b"\r\n\r\n", 1)
                    length = next(int(line.split(b":", 1)[1]) for line in headers.split(b"\r\n")
                                  if line.lower().startswith(b"content-length:"))
                    while len(body) < length:
                        part = connection.recv(8192)
                        if not part:
                            return
                        body += part
                    requests.append(request)
                    valid = request.startswith(b"POST /tool HTTP/1.1\r\n") and \
                        b"Authorization: Bearer synthetic-file-capability\r\n" in request and \
                        json.loads(body) == {"command": "work-context", "input": {}}
                    body = json.dumps({"enabled": valid, "work_id": "fictional-work", "inputs": [],
                                       "output_dir": str(self.work / "outputs")}).encode()
                    connection.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: " +
                        str(len(body)).encode() + b"\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n" + body)
            except (OSError, TimeoutError):
                pass

        server = threading.Thread(target=serve_tool, daemon=True)
        server.start()
        probe = self.work / "inputs" / "probe.py"
        # The untrusted command knows every host pathname and PID. Path secrecy
        # is therefore not what makes these assertions pass.
        probe.write_text(PROBE.replace("FIXTURE", repr({
            "work": str(self.work), "host_paths": [str(p) for p in host_paths],
            "pid": sibling.pid, "port": host_tcp.getsockname()[1]})), encoding="utf-8")
        result = self.invoke(self.command, "-I", "-B", str(probe))
        self.assertEqual(result.returncode, 0, "Linux namespace probe failed: " + result.stderr.strip())
        report = json.loads((self.work / "outputs" / "report.json").read_text())
        self.assertTrue(report)
        for name, value in report.items():
            with self.subTest(boundary=name):
                self.assertIs(value, True)
        server.join(timeout=2)
        self.assertEqual(len(requests), 1)
        self.assertEqual((self.work / "inputs" / "input.txt").read_text(), "synthetic-input")
        self.assertFalse((self.root / "escape-marker").exists())


PROBE = r'''
import json, os, socket, subprocess
from pathlib import Path
fixture = FIXTURE
work = Path(fixture["work"])
checks = {"input_visible": (work / "inputs/input.txt").read_text() == "synthetic-input"}
try:
    (work / "inputs/input.txt").write_text("mutation")
    checks["input_readonly"] = False
except OSError:
    checks["input_readonly"] = True
checks["host_paths_hidden"] = all(not Path(path).exists() for path in fixture["host_paths"])
checks["host_root_detached"] = not Path("/.oldroot").exists()
checks["host_proc_hidden"] = not Path(f"/proc/{fixture['pid']}").exists()
checks["private_pid_namespace"] = os.getpid() == 1
checks["private_hostname"] = socket.gethostname() == "file-work"
checks["host_environment_hidden"] = "HOST_ONLY_SENTINEL" not in os.environ and "PYTHONPATH" not in os.environ
checks["scoped_capability_only"] = os.environ.get("CC_FILE_ACTION_TOKEN") == "synthetic-file-capability" and os.environ.get("MYANC_CRM_ACTION_TOKEN") == "synthetic-file-capability"
status = Path("/proc/self/status").read_text().splitlines()
checks["capabilities_dropped"] = all(int(line.split()[1], 16) == 0 for line in status if line.startswith(("CapEff:", "CapPrm:", "CapBnd:")))
checks["no_new_privileges"] = "NoNewPrivs:\t1" in status
Path(os.environ["HOME"], "private.txt").write_text("private")
Path(os.environ["TMPDIR"], "private.txt").write_text("private")
(work.parent.parent / "escape-marker").write_text("private-only")
checks["private_home_and_tmp_writable"] = True
with socket.socket() as connection:
    connection.settimeout(1)
    try:
        connection.connect(("127.0.0.1", fixture["port"]))
        checks["host_network_hidden"] = False
    except OSError:
        checks["host_network_hidden"] = True
client = subprocess.run(["cc-connect-file", "work-context"], input="{}", text=True,
                        capture_output=True, timeout=5)
checks["restricted_tool_socket_usable"] = client.returncode == 0 and json.loads(client.stdout).get("work_id") == "fictional-work"
(work / "outputs/report.json").write_text(json.dumps(checks))
'''


if __name__ == "__main__":
    unittest.main()
