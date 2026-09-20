"""Offline file-client contract, using a fake Unix socket and real HTTP parser."""
import io
import json
import os
import socket
import unittest
from unittest.mock import patch

import file_tool


class Peer:
    def __init__(self, result, status=200):
        body = json.dumps(result).encode()
        self.response = (f"HTTP/1.1 {status} Fixture\r\nContent-Type: application/json\r\n"
                         f"Content-Length: {len(body)}\r\n\r\n").encode() + body
        self.sent = bytearray()
        self.address = None
        self.closed = False

    def settimeout(self, timeout):
        self.timeout = timeout

    def connect(self, address):
        self.address = address

    def sendall(self, data):
        self.sent.extend(data)

    def makefile(self, *_args):
        return io.BytesIO(self.response)

    def close(self):
        self.closed = True


class FileClientTests(unittest.TestCase):
    def invoke(self, command, raw, peer, env=None):
        output = io.StringIO()
        environment = {"CC_FILE_ACTION_TOKEN": "fixture-token",
                       "CC_ACTION_TOOLS_SOCKET": file_tool.SOCKET}
        if env:
            environment.update(env)
        with (patch.dict(os.environ, environment, clear=True),
              patch.object(socket, "AF_UNIX", getattr(socket, "AF_UNIX", 99), create=True),
              patch.object(socket, "socket", return_value=peer) as factory):
            code = file_tool.main(command, stdin=io.BytesIO(raw), stdout=output)
        return code, json.loads(output.getvalue()), factory

    def test_only_fixed_socket_and_business_envelope_are_sent(self):
        expected = {"enabled": True, "work_id": "fictional", "inputs": [], "output_dir": "/fixture/outputs"}
        peer = Peer(expected)
        code, result, factory = self.invoke(["work-context"], b"{}", peer)
        self.assertEqual((0, expected), (code, result))
        factory.assert_called_once_with(getattr(socket, "AF_UNIX", 99), socket.SOCK_STREAM)
        self.assertEqual(file_tool.SOCKET, peer.address)
        headers, body = bytes(peer.sent).split(b"\r\n\r\n", 1)
        self.assertTrue(headers.startswith(b"POST /tool HTTP/1.1\r\n"))
        self.assertIn(b"Authorization: Bearer fixture-token", headers)
        self.assertEqual({"command": "work-context", "input": {}}, json.loads(body))
        self.assertTrue(peer.closed)

    def test_overrides_invalid_json_and_missing_capability_never_connect(self):
        cases = [(["send"], b"{}", None), (["work-context", "--socket", "/other"], b"{}", None),
                 (["work-context"], b'{"recipient":"other"}', None),
                 (["file-status"], b'{"work_id":"a","delivery_id":"b","root":"/other"}', None),
                 (["work-context"], b'{"x":1,"x":2}', None),
                 (["work-context"], b'{} {}', None), (["work-context"], b'null', None),
                 (["work-context"], b'{}', {"CC_FILE_ACTION_TOKEN": "bad\r\nsecret"}),
                 (["work-context"], b'{}', {"CC_ACTION_TOOLS_SOCKET": "/other/socket"}),
                 (["work-context"], b'{}', {"CC_ACTION_TOOLS_SOCKET": ""})]
        for command, raw, env in cases:
            with self.subTest(command=command, raw_size=len(raw)):
                code, _, factory = self.invoke(command, raw, Peer({}), env)
                self.assertNotEqual(0, code)
                factory.assert_not_called()

    def test_durable_statuses_are_preserved_and_network_is_never_retried(self):
        raw = b'{"work_id":"fictional","delivery_id":"delivery-a"}'
        for status in ("submitted", "accepted", "failed", "unknown"):
            expected = {"delivery_id": "delivery-a", "status": status, "version": 1}
            if status == "accepted":
                expected["message_receipt"] = "message-a"
            code, result, factory = self.invoke(["file-status"], raw, Peer(expected))
            self.assertEqual((0, expected), (code, result))
            self.assertEqual(1, factory.call_count)
        peer = Peer({})
        peer.sendall = lambda _data: (_ for _ in ()).throw(OSError("private secret path"))
        code, result, factory = self.invoke(["file-status"], raw, peer)
        self.assertEqual((1, {"status": "unavailable", "code": "outcome_unconfirmed"}), (code, result))
        self.assertEqual(1, factory.call_count)
        self.assertTrue(peer.closed)

    def test_redirect_and_false_acceptance_are_not_receipts(self):
        for peer in (Peer({"status": "accepted"}, 302), Peer({"status": "accepted"}),
                     Peer({"status": "accepted"}, 500), Peer({"enabled": False})):
            code, result, factory = self.invoke(["work-context"], b"{}", peer)
            self.assertEqual(1, code)
            self.assertEqual("invalid_response", result["code"])
            self.assertEqual(1, factory.call_count)


if __name__ == "__main__":
    unittest.main()
