"""Offline file-client contract, using a fake Unix socket and real HTTP parser."""
import base64
import hashlib
import io
import json
import os
import socket
import stat
import tempfile
from pathlib import Path
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

    def setsockopt(self, *_args):
        pass

    def connect(self, address):
        self.address = address

    def sendall(self, data):
        self.sent.extend(data)

    def makefile(self, *_args):
        return io.BytesIO(self.response)

    def close(self):
        self.closed = True


class FileClientTests(unittest.TestCase):
    @unittest.skipUnless(os.name == 'posix', 'POSIX ownership and no-follow publication')
    def test_only_selected_owned_output_is_published_and_links_digest_fail_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            previous = os.getcwd()
            try:
                os.chdir(directory)
                Path('outputs/nested').mkdir(parents=True, mode=0o700)
                selected = Path('outputs/nested/result.docx')
                selected.write_bytes(b'fictional-docx-client-fixture')
                selected.chmod(0o600)
                untouched = Path('outputs/private.txt')
                untouched.write_bytes(b'private-draft')
                untouched.chmod(0o600)
                value = {'work_id':'fixture', 'path':'nested/result.docx',
                         'sha256':hashlib.sha256(selected.read_bytes()).hexdigest(), 'expected_version':0}
                accepted = {'delivery_id':'fixture-delivery', 'status':'accepted', 'message_receipt':'fixture-message'}
                code, result, factory = self.invoke(['file-deliver'], json.dumps(value).encode(), Peer(accepted))
                self.assertEqual((code,result), (0,accepted))
                self.assertEqual(factory.call_count, 1)
                self.assertEqual(stat.S_IMODE(selected.stat().st_mode), 0o640)
                self.assertEqual(stat.S_IMODE(selected.parent.stat().st_mode), 0o2750)
                self.assertEqual(stat.S_IMODE(untouched.stat().st_mode), 0o600)
                for kind in ('digest', 'symlink', 'hardlink'):
                    with self.subTest(kind=kind):
                        bad = dict(value)
                        selected.chmod(0o600)
                        if kind == 'digest':
                            bad['sha256'] = '0' * 64
                        else:
                            link = Path('outputs/' + kind)
                            if kind == 'symlink':
                                link.symlink_to('nested/result.docx')
                            else:
                                os.link(selected, link)
                            bad['path'] = kind
                        code, result, factory = self.invoke(['file-deliver'], json.dumps(bad).encode(), Peer(accepted))
                        self.assertEqual((code,result['code']), (2,'file_access_not_prepared'))
                        factory.assert_not_called()
                        self.assertEqual(stat.S_IMODE(selected.stat().st_mode), 0o600)
            finally:
                os.chdir(previous)

    def invoke(self, command, raw, peer, env=None):
        output = io.StringIO()
        environment = {"CC_FILE_ACTION_TOKEN": "fixture-token",
                       "CC_ACTION_TOOLS_SOCKET": file_tool.SOCKET}
        if env:
            environment.update(env)
        with (patch.dict(os.environ, environment, clear=True),
              patch.object(socket, "AF_UNIX", getattr(socket, "AF_UNIX", 99), create=True),
              patch.object(socket, "create_connection", return_value=peer),
              patch.object(socket, "socket", return_value=peer) as factory):
            code = file_tool.main(command, stdin=io.BytesIO(raw), stdout=output)
        return code, json.loads(output.getvalue()), factory

    def test_native_uses_only_fixed_endpoint_through_existing_proxy(self):
        expected = {"enabled": True, "work_id": "fictional", "inputs": [], "output_dir": "/fixture/outputs"}
        peer = Peer(expected)
        with patch.object(socket, "create_connection", return_value=peer) as connect:
            with patch.dict(os.environ, {"CC_FILE_ACTION_TOKEN": "fixture-token",
                    "CC_FILE_TRANSPORT": "native", "HTTP_PROXY": "http://127.0.0.1:43123",
                    "NO_PROXY": "*"}, clear=True):
                output = io.StringIO()
                code = file_tool.main(["work-context"], stdin=io.BytesIO(b'{}'), stdout=output)
        self.assertEqual((code, json.loads(output.getvalue())), (0, expected))
        self.assertEqual(connect.call_args.args[0], ("127.0.0.1", 43123))
        self.assertTrue(bytes(peer.sent).startswith(b'POST http://127.0.0.1:18743/tool HTTP/1.1'))
        self.assertIn(b'Authorization: Bearer fixture-token', peer.sent)
        self.assertTrue(peer.closed)
        for proxy in ('', 'http://remote.invalid:80', 'https://127.0.0.1:80',
                      'http://user@localhost:80', 'http://user:@localhost:80',
                      'http://user:%0d%0asecret@localhost:80', 'http://user:password@remote.invalid:80',
                      'http://localhost:80/other',
                      'http://localhost:80?endpoint=other', 'http://localhost:99999'):
            code, result, factory = self.invoke(['work-context'], b'{}', Peer({}),
                {'CC_FILE_TRANSPORT':'native', 'HTTP_PROXY':proxy})
            self.assertEqual((code,result['code']), (1,'file_transport_disabled'))
            factory.assert_not_called()

    def test_native_authenticated_proxy_keeps_proxy_and_session_credentials_separate(self):
        expected = {"enabled": True, "work_id": "fictional", "inputs": [], "output_dir": "/fixture/outputs"}
        peer = Peer(expected)
        with (patch.object(socket, "create_connection", return_value=peer) as connect,
              patch.dict(os.environ, {'CC_FILE_ACTION_TOKEN': 'fixture-token',
                  'CC_FILE_TRANSPORT': 'native', 'HTTP_PROXY': 'http://srt:fixture%2Bproxy@127.0.0.1:43123',
                  'NO_PROXY': '*'}, clear=True)):
            output = io.StringIO()
            code = file_tool.main(["work-context"], stdin=io.BytesIO(b'{}'), stdout=output)
            result = json.loads(output.getvalue())
        self.assertEqual((code, result), (0, expected))
        request = bytes(peer.sent)
        self.assertEqual(connect.call_args.args[0], ("127.0.0.1", 43123))
        self.assertTrue(request.startswith(b'POST http://127.0.0.1:18743/tool HTTP/1.1'))
        self.assertIn(b'Proxy-Authorization: Basic ' + base64.b64encode(b'srt:fixture+proxy'), request)
        self.assertIn(b'Authorization: Bearer fixture-token', request)
        self.assertNotIn(b'fixture%2Bproxy', request)
        self.assertTrue(peer.closed)

    @unittest.skipUnless(os.name == 'posix', 'POSIX publication')
    def test_native_publishes_selected_work_with_employee_cwd_unchanged(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            (root / 'outputs').mkdir()
            artifact = root / 'outputs' / 'result.docx'
            artifact.write_bytes(b'fictional-output')
            artifact.chmod(0o600)
            previous = os.getcwd()
            with patch.dict(os.environ, {'CC_FILE_TRANSPORT':'native', 'CC_FILE_WORK_ROOT':str(root)}):
                file_tool.prepare_delivery({'path':'result.docx', 'sha256':hashlib.sha256(artifact.read_bytes()).hexdigest()})
                self.assertEqual(stat.S_IMODE(artifact.stat().st_mode), 0o640)
                self.assertEqual(previous, os.getcwd())
            with patch.dict(os.environ, {'CC_FILE_TRANSPORT':'native', 'CC_FILE_WORK_ROOT':''}):
                with self.assertRaises(ValueError):
                    file_tool.prepare_delivery({'path':'result.docx'})

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
