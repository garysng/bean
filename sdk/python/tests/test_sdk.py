"""Unit tests for the bean SDK against a stub HTTP server."""

import json
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from bean import BeanAPIError, BeanClient, BeanConnectionError, Event  # noqa: E402


class StubHandler(BaseHTTPRequestHandler):
    store = {}
    protocol_version = "HTTP/1.0"  # no keep-alive: avoids teardown races

    def log_message(self, *a):
        pass

    def handle_one_request(self):
        # Swallow connection resets during shutdown so they do not surface as
        # spurious test errors.
        try:
            super().handle_one_request()
        except (ConnectionResetError, BrokenPipeError):
            self.close_connection = True

    def _auth_ok(self):
        return self.headers.get("Authorization") == "Bearer test-key"

    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        if not self._auth_ok():
            return self._json(401, {"error": {"code": "UNAUTHENTICATED", "message": "no"}})
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")
        if self.path == "/v1/sandboxes":
            if not body.get("image"):
                return self._json(400, {"error": {"code": "IMAGE_REF_INVALID", "message": "image required"}})
            sb = {"id": "sbx_stub1", "state": "RUNNING", "image": body["image"],
                  "labels": body.get("labels", {})}
            StubHandler.store["sbx_stub1"] = sb
            return self._json(201, {"sandbox": sb})
        if self.path.endswith("/exec"):
            cmd = body["cmd"]
            return self._json(200, {"exitCode": 0, "stdout": " ".join(cmd), "stderr": "",
                                    "truncated": False, "durationMs": 5})
        if self.path.endswith("/pause") or self.path.endswith("/resume"):
            self.send_response(202)
            self.end_headers()
            return
        self._json(404, {"error": {"code": "SANDBOX_NOT_FOUND", "message": "nope"}})

    def do_GET(self):
        if not self._auth_ok():
            return self._json(401, {"error": {"code": "UNAUTHENTICATED", "message": "no"}})
        if self.path.startswith("/v1/events"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            # Preamble comment, two real events, a keepalive, then EOF.
            self.wfile.write(b": connected\n\n")
            for typ, sid in (("sandbox.lifecycle.created", "sbx_1"),
                             ("sandbox.lifecycle.running", "sbx_1")):
                payload = json.dumps({"type": typ, "sandboxId": sid,
                                      "timestamp": "2026-08-01T00:00:00Z",
                                      "data": {"k": "v"}, "version": "v1"})
                self.wfile.write(f"event: {typ}\ndata: {payload}\n\n".encode())
            self.wfile.write(b": keepalive\n\n")
            self.wfile.flush()
            return
        if self.path == "/v1/sandboxes":
            return self._json(200, {"sandboxes": list(StubHandler.store.values())})
        if self.path.startswith("/v1/sandboxes/sbx_stub1/files?"):
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"file-content")
            return
        if self.path.startswith("/v1/sandboxes/sbx_stub1"):
            return self._json(200, {"sandbox": StubHandler.store.get("sbx_stub1")})
        self._json(404, {"error": {"code": "SANDBOX_NOT_FOUND", "message": "nope"}})

    def do_PUT(self):
        length = int(self.headers.get("Content-Length", 0))
        data = self.rfile.read(length)
        self._json(200, {"bytesWritten": len(data)})

    def do_DELETE(self):
        self.send_response(202)
        self.end_headers()


class SDKTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.httpd = HTTPServer(("127.0.0.1", 0), StubHandler)
        threading.Thread(target=cls.httpd.serve_forever, daemon=True).start()
        cls.client = BeanClient(api_key="test-key",
                                base_url=f"http://127.0.0.1:{cls.httpd.server_port}")

    @classmethod
    def tearDownClass(cls):
        cls.httpd.shutdown()
        cls.httpd.server_close()

    def test_create_and_exec(self):
        sb = self.client.sandboxes.create(image="python:3.12", labels={"a": "b"})
        self.assertEqual(sb.id, "sbx_stub1")
        self.assertEqual(sb.state, "RUNNING")
        r = sb.exec(["echo", "hi"])
        self.assertEqual(r.exit_code, 0)
        self.assertEqual(r.stdout, "echo hi")

    def test_exec_str_wraps_shell(self):
        sb = self.client.sandboxes.create(image="x")
        r = sb.exec("echo hi")
        self.assertEqual(r.stdout, "/bin/sh -c echo hi")

    def test_files(self):
        sb = self.client.sandboxes.create(image="x")
        n = sb.write_file("/a.txt", "hello")
        self.assertEqual(n, 5)
        self.assertEqual(sb.read_file("/a.txt"), b"file-content")

    def test_error_mapping(self):
        bad = BeanClient(api_key="wrong",
                         base_url=self.client.base_url)
        with self.assertRaises(BeanAPIError) as cm:
            bad.sandboxes.list()
        self.assertEqual(cm.exception.code, "UNAUTHENTICATED")
        self.assertEqual(cm.exception.http_status, 401)

    def test_create_validation_error(self):
        with self.assertRaises(BeanAPIError) as cm:
            self.client.sandboxes.create(image="")
        self.assertEqual(cm.exception.code, "IMAGE_REF_INVALID")

    def test_context_manager_kills(self):
        with self.client.sandboxes.create(image="x") as sb:
            self.assertEqual(sb.state, "RUNNING")
        self.assertEqual(sb.state, "STOPPED")


    def test_connection_error_is_wrapped(self):
        # 192.0.2.0/24 is TEST-NET-1 (RFC 5737): guaranteed unroutable, so the
        # request fails at the transport layer rather than hitting a live port.
        c = BeanClient(api_key="k", base_url="http://192.0.2.1:8080", timeout=1)
        with self.assertRaises(BeanConnectionError) as cm:
            c.sandboxes.list()
        self.assertEqual(cm.exception.code, "CONNECTION_ERROR")
        self.assertIsInstance(cm.exception, BeanAPIError)

    def test_events_subscribe_yields_events(self):
        got = list(self.client.events.subscribe())
        if len(got) != 2:
            self.fail(f"expected 2 events, got {got}")
        self.assertIsInstance(got[0], Event)
        self.assertEqual(got[0].type, "sandbox.lifecycle.created")
        self.assertEqual(got[0].sandbox_id, "sbx_1")
        self.assertEqual(got[0].data, {"k": "v"})
        self.assertEqual(got[1].type, "sandbox.lifecycle.running")

    def test_events_subscribe_filters_are_sent(self):
        # Filters go on the query string; the stub accepts any and replays.
        got = list(self.client.events.subscribe(sandbox_id="sbx_1",
                                                labels={"eval-run": "r1"}))
        self.assertEqual(len(got), 2)

    def test_events_subscribe_auth_error(self):
        bad = BeanClient(api_key="wrong", base_url=self.client.base_url)
        with self.assertRaises(BeanAPIError) as cm:
            list(bad.events.subscribe())
        self.assertEqual(cm.exception.code, "UNAUTHENTICATED")

    def test_events_subscribe_connection_error(self):
        c = BeanClient(api_key="k", base_url="http://192.0.2.1:8080")
        with self.assertRaises(BeanConnectionError):
            list(c.events.subscribe(timeout=1))

    def test_timeout_configurable(self):
        c = BeanClient(api_key="k", base_url=self.client.base_url, timeout=1.5)
        self.assertEqual(c.timeout, 1.5)


if __name__ == "__main__":
    unittest.main()
