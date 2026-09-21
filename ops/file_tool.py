#!/usr/bin/env python3
"""Narrow model-side file client; no management API, credentials or recipients."""

import http.client
import hashlib
import json
import os
import re
import socket
import stat
import sys
import urllib.parse

SOCKET = "/run/cc-connect/action-tools.sock"
ENDPOINT = "http://127.0.0.1:18743/tool"
MAX_INPUT = 4096
MAX_RESPONSE = 1024 * 1024
FIELDS = {
    "work-context": set(),
    "file-deliver": {"work_id", "path", "sha256", "expected_version"},
    "file-status": {"work_id", "delivery_id"},
}


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate field")
        result[key] = value
    return result


def reject_constant(_value):
    raise ValueError("non-JSON number")


def decode(raw):
    return json.loads(raw.decode("utf-8"), object_pairs_hook=unique_object,
                      parse_constant=reject_constant)


class SessionHTTP(http.client.HTTPConnection):
    def connect(self):
        if not hasattr(socket, "AF_UNIX"):
            raise OSError("Unix transport unavailable")
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(SOCKET)


def transport():
    if os.environ.get("CC_FILE_TRANSPORT") == "native":
        # Use the existing Claude sandbox proxy, including for loopback. Do not
        # honor NO_PROXY or follow redirects carrying the session capability.
        proxy = os.environ.get("http_proxy") or os.environ.get("HTTP_PROXY", "")
        parsed = urllib.parse.urlsplit(proxy)
        if (parsed.scheme != "http" or parsed.hostname not in {"127.0.0.1", "localhost", "::1"}
                or not parsed.port or parsed.username is not None or parsed.password is not None
                or parsed.path not in {"", "/"} or parsed.query or parsed.fragment
                or any(ord(c) <= 32 or ord(c) >= 127 for c in proxy)):
            raise ValueError("native loopback proxy required")
        return http.client.HTTPConnection(parsed.hostname, parsed.port, timeout=35), ENDPOINT
    if os.environ.get("CC_FILE_TRANSPORT", "") or os.environ.get("CC_ACTION_TOOLS_SOCKET") != SOCKET:
        raise ValueError("file transport disabled")
    return SessionHTTP("localhost", timeout=35), "/tool"


def emit(stdout, result, code):
    print(json.dumps(result, ensure_ascii=False), file=stdout)
    return code


def valid_input(command, value):
    if not isinstance(value, dict) or set(value) != FIELDS[command]:
        return False
    for name in ("work_id", "delivery_id"):
        if name in value and (not isinstance(value[name], str) or not value[name]
                              or len(value[name]) > 256):
            return False
    if command == "file-deliver":
        path = value["path"]
        return (type(value["expected_version"]) is int and value["expected_version"] >= 0
                and isinstance(value["sha256"], str)
                and re.fullmatch(r"[0-9a-f]{64}", value["sha256"]) is not None
                and isinstance(path, str) and bool(path)
                and not any(c in path for c in "\\:\x00")
                and all(part not in {"", ".", ".."} for part in path.split("/")))
    return True


def prepare_delivery(value):
    """Grant the host group access only to the selected, verified output.

    The sandbox fixes cwd and outputs. The host independently checks ownership,
    no-follow paths, digest and the immutable snapshot; this client is not an
    authorization boundary and cannot select a recipient or a host directory.
    """
    flags = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK
    output = "outputs"
    if os.environ.get("CC_FILE_TRANSPORT") == "native":
        root = os.environ.get("CC_FILE_WORK_ROOT", "")
        if not os.path.isabs(root) or os.path.realpath(root) != root:
            raise ValueError("invalid native work root")
        output = os.path.join(root, "outputs")
    directory = os.open(output, flags | os.O_DIRECTORY)
    try:
        group = os.fstat(directory).st_gid
        parts = value["path"].split("/")
        for part in parts[:-1]:
            child = os.open(part, flags | os.O_DIRECTORY, dir_fd=directory)
            os.close(directory)
            directory = child
            info = os.fstat(directory)
            if info.st_uid != os.geteuid() or info.st_gid != group:
                raise ValueError("invalid output directory")
            os.fchmod(directory, 0o2750)
        fd = os.open(parts[-1], flags, dir_fd=directory)
        with os.fdopen(fd, "rb") as stream:
            info = os.fstat(stream.fileno())
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid()
                    or info.st_gid != group or info.st_nlink != 1 or not 0 < info.st_size <= 20 << 20):
                raise ValueError("invalid output file")
            data = stream.read((20 << 20) + 1)
            if len(data) != info.st_size or hashlib.sha256(data).hexdigest() != value["sha256"]:
                raise ValueError("output changed")
            os.fchmod(stream.fileno(), 0o640)
    finally:
        os.close(directory)


def main(argv=None, *, stdin=None, stdout=None):
    argv = sys.argv[1:] if argv is None else argv
    stdin = sys.stdin.buffer if stdin is None else stdin
    stdout = sys.stdout if stdout is None else stdout
    if len(argv) != 1 or argv[0] not in FIELDS:
        return emit(stdout, {"status": "blocked", "code": "invalid_command"}, 2)
    token = os.environ.get("CC_FILE_ACTION_TOKEN", "")
    if not token or len(token) > 1024 or any(ord(c) <= 32 or ord(c) >= 127 for c in token):
        return emit(stdout, {"status": "blocked", "code": "invalid_session"}, 2)
    try:
        conn, target = transport()
    except ValueError:
        return emit(stdout, {"status": "unavailable", "code": "file_transport_disabled"}, 1)
    try:
        raw = stdin.read(MAX_INPUT + 1)
        if isinstance(raw, str):
            raw = raw.encode("utf-8")
        if len(raw) > MAX_INPUT:
            raise ValueError("input too large")
        value = decode(raw)
        if not valid_input(argv[0], value):
            raise ValueError("invalid business input")
        body = json.dumps({"command": argv[0], "input": value}).encode("utf-8")
    except (ValueError, OSError, RecursionError):
        return emit(stdout, {"status": "blocked", "code": "invalid_input"}, 2)

    if argv[0] == "file-deliver":
        try:
            prepare_delivery(value)
        except (ValueError, OSError, AttributeError):
            return emit(stdout, {"status": "blocked", "code": "file_access_not_prepared"}, 2)

    try:
        conn.request("POST", target, body, {"Authorization": "Bearer " + token,
                                            "Content-Type": "application/json"})
        response = conn.getresponse()
        raw = response.read(MAX_RESPONSE + 1)
        if len(raw) > MAX_RESPONSE or response.headers.get_content_type() != "application/json":
            raise ValueError("invalid response")
        result = decode(raw)
        if not isinstance(result, dict):
            raise ValueError("invalid response")
        if response.status != 200:
            if response.status < 400 or result.get("status") not in {"blocked", "unavailable"}:
                raise ValueError("unexpected response")
            return emit(stdout, result, 1)
        if argv[0] == "work-context":
            if (result.get("enabled") is not True or not isinstance(result.get("work_id"), str)
                    or not result["work_id"] or not isinstance(result.get("inputs"), list)
                    or not isinstance(result.get("output_dir"), str)):
                raise ValueError("invalid work context")
        elif (result.get("status") not in {"submitted", "accepted", "failed", "unknown"}
              or not isinstance(result.get("delivery_id"), str) or not result["delivery_id"]
              or (result["status"] == "accepted" and not result.get("message_receipt"))):
            raise ValueError("invalid receipt")
        return emit(stdout, result, 0)
    except (OSError, http.client.HTTPException):
        return emit(stdout, {"status": "unavailable", "code": "outcome_unconfirmed"}, 1)
    except (ValueError, RecursionError):
        return emit(stdout, {"status": "unavailable", "code": "invalid_response"}, 1)
    finally:
        conn.close()


if __name__ == "__main__":
    raise SystemExit(main())
