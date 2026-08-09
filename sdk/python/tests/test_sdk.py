"""Unit tests for the bean SDK against a stub HTTP server."""

import json
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from bean import (  # noqa: E402
    BeanAPIError, BeanClient, BeanConnectionError, Event, Snapshot,
)


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
            image, snap = body.get("image"), body.get("snapshot")
            if not image and not snap:
                return self._json(400, {"error": {"code": "INVALID_ARGUMENT",
                                                  "message": "image or snapshot required"}})
            if image == "reject-me":
                return self._json(400, {"error": {"code": "IMAGE_REF_INVALID",
                                                  "message": "image rejected"}})
            sb = {"id": "sbx_stub1", "state": "RUNNING",
                  "image": image or "python:3.12",
                  "snapshotId": snap or "",
                  "labels": body.get("labels", {})}
            StubHandler.store["sbx_stub1"] = sb
            return self._json(201, {"sandbox": sb})
        if self.path.endswith("/exec"):
            cmd = body["cmd"]
            return self._json(200, {"exitCode": 0, "stdout": " ".join(cmd), "stderr": "",
                                    "truncated": False, "durationMs": 5})
        if self.path.endswith("/snapshot"):
            return self._json(202, {
                "snapshotId": "snap_stub1",
                "snapshot": {"id": "snap_stub1", "state": "READY",
                             "sandboxId": "sbx_stub1", "image": "python:3.12",
                             "name": body.get("name", ""), "sizeBytes": 2048},
            })
        if self.path.endswith("/commit"):
            return self._json(201, {"imageRef": body["tag"]})
        if self.path == "/v1/images/build":
            return self._json(202, {"imageRef": body["tag"], "nodeId": "node-a",
                                    "state": "BUILDING",
                                    "hadContext": bool(body.get("contextTar"))})
        if self.path == "/v1/images/prewarm":
            return self._json(202, {"jobId": "pw_stub1",
                                    "ready": {r: 1 for r in body.get("refs", [])}})
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
        if self.path.startswith("/v1/snapshots/"):
            return self._json(200, {"snapshot": {
                "id": "snap_stub1", "state": "READY", "sandboxId": "sbx_stub1",
                "image": "python:3.12", "sizeBytes": 2048}})
        if self.path.startswith("/v1/snapshots"):
            return self._json(200, {"snapshots": [{
                "id": "snap_stub1", "state": "READY", "sandboxId": "sbx_stub1",
                "image": "python:3.12", "sizeBytes": 2048}]})
        if self.path == "/v1/images":
            return self._json(200, {"images": [
                {"ref": "python:3.12", "state": "PENDING", "cachedNodes": 0}]})
        if self.path.startswith("/v1/images/status"):
            return self._json(200, {"ref": "python:3.12", "state": "PENDING",
                                    "format": "oci", "cachedNodes": 0})
        if self.path.startswith("/v1/images/prewarm/"):
            return self._json(200, {"jobId": "pw_stub1", "done": True})
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
        # A server-side rejection surfaces as BeanAPIError with its code.
        with self.assertRaises(BeanAPIError) as cm:
            self.client.sandboxes.create(image="reject-me")
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

    def test_sandbox_snapshot(self):
        sb = self.client.sandboxes.create(image="python:3.12")
        snap = sb.snapshot(name="after-setup")
        self.assertIsInstance(snap, Snapshot)
        self.assertEqual(snap.id, "snap_stub1")
        self.assertEqual(snap.state, "READY")
        self.assertEqual(snap.size_bytes, 2048)
        # Keeping the sandbox running is the default.
        self.assertEqual(sb.state, "RUNNING")

    def test_snapshot_stops_source_when_asked(self):
        sb = self.client.sandboxes.create(image="python:3.12")
        sb.snapshot(keep_running=False)
        self.assertEqual(sb.state, "STOPPED")

    def test_commit_returns_image_ref_and_keeps_sandbox_running(self):
        sb = self.client.sandboxes.create(image="python:3.12")
        ref = sb.commit("myteam/prepared:v1")
        self.assertEqual(ref, "myteam/prepared:v1")
        # Unlike snapshot(keep_running=False), commit never stops the source:
        # freezing the filesystem does not end the session.
        self.assertEqual(sb.state, "RUNNING")

    def test_build_accepts_dockerfile_without_a_context(self):
        out = self.client.images.build(
            tag="myteam/app:v1",
            dockerfile="FROM alpine:3.20\nRUN echo hi\n",
        )
        self.assertEqual(out["imageRef"], "myteam/app:v1")
        self.assertEqual(out["state"], "BUILDING")
        # A Dockerfile that only runs commands should not upload anything.
        self.assertFalse(out["hadContext"])

    def test_build_packs_context_directory(self):
        import os
        import tempfile
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "app.py"), "w") as f:
                f.write("print('hi')\n")
            # .git must never be shipped: large, useless to a build, and it
            # would put the repository history in the context.
            os.makedirs(os.path.join(d, ".git"))
            with open(os.path.join(d, ".git", "config"), "w") as f:
                f.write("secret\n")
            out = self.client.images.build(
                tag="myteam/withctx:v1",
                dockerfile="FROM alpine:3.20\nCOPY app.py /app.py\n",
                context_dir=d,
            )
        self.assertTrue(out["hadContext"])

    def test_create_from_snapshot(self):
        sb = self.client.sandboxes.create(snapshot="snap_stub1")
        self.assertEqual(sb.id, "sbx_stub1")

    def test_create_requires_exactly_one_source(self):
        with self.assertRaises(ValueError):
            self.client.sandboxes.create(image="x", snapshot="y")
        with self.assertRaises(ValueError):
            self.client.sandboxes.create()

    def test_snapshots_namespace(self):
        snaps = self.client.snapshots.list()
        self.assertEqual(len(snaps), 1)
        self.assertEqual(snaps[0].id, "snap_stub1")
        one = self.client.snapshots.get("snap_stub1")
        self.assertEqual(one.image, "python:3.12")
        # Deleting through either the namespace or the object works.
        self.client.snapshots.delete("snap_stub1")
        one.delete()

    def test_images_namespace(self):
        imgs = self.client.images.list()
        self.assertEqual(imgs[0]["ref"], "python:3.12")
        status = self.client.images.status("python:3.12")
        self.assertEqual(status["format"], "oci")
        job = self.client.images.prewarm(["python:3.12"], target_nodes=1)
        self.assertEqual(job["jobId"], "pw_stub1")
        self.assertEqual(job["ready"]["python:3.12"], 1)
        self.assertTrue(self.client.images.prewarm_status("pw_stub1")["done"])

    def test_timeout_configurable(self):
        c = BeanClient(api_key="k", base_url=self.client.base_url, timeout=1.5)
        self.assertEqual(c.timeout, 1.5)


import base64  # noqa: E402
import struct  # noqa: E402


class ProxyHandler(BaseHTTPRequestHandler):
    """A stub bean-proxy speaking Connect HTTP/JSON, enough for the SDK's data
    plane: unary Exec as JSON, ReadFile/WriteFile as enveloped streams. It
    records the Host header so the test can assert the SDK addresses the agent
    as {port}-{sandbox}.{domain}."""

    last_host = None
    protocol_version = "HTTP/1.0"

    def log_message(self, *a):
        pass

    def _read_body(self):
        length = int(self.headers.get("Content-Length", 0))
        return self.rfile.read(length)

    @staticmethod
    def _envelope(payload: bytes, end: bool = False) -> bytes:
        flags = 0x02 if end else 0x00
        return struct.pack(">BI", flags, len(payload)) + payload

    def do_POST(self):
        ProxyHandler.last_host = self.headers.get("Host")
        raw = self._read_body()
        if self.path.endswith("/Exec"):
            req = json.loads(raw)
            out = base64.b64encode((" ".join(req["cmd"])).encode()).decode()
            body = json.dumps({"exitCode": 0, "stdout": out, "stderr": "",
                               "truncated": False, "durationMs": 7}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(body)
            return
        if self.path.endswith("/ReadFile"):
            chunk = json.dumps({"data": base64.b64encode(b"proxied-bytes").decode()}).encode()
            trailer = json.dumps({}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/connect+json")
            self.end_headers()
            self.wfile.write(self._envelope(chunk))
            self.wfile.write(self._envelope(trailer, end=True))
            return
        if self.path.endswith("/WriteFile"):
            # Sum the data frames the client sent, echo it back as bytesWritten.
            total = 0
            off = 0
            while off + 5 <= len(raw):
                flags, length = struct.unpack(">BI", raw[off:off + 5])
                off += 5
                frame = json.loads(raw[off:off + length] or b"{}")
                off += length
                if "data" in frame:
                    total += len(base64.b64decode(frame["data"]))
            resp = json.dumps({"bytesWritten": total}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/connect+json")
            self.end_headers()
            self.wfile.write(self._envelope(resp))
            self.wfile.write(self._envelope(b"{}", end=True))
            return
        self.send_response(404)
        self.end_headers()


class DataPlaneTest(unittest.TestCase):
    """The SDK reaches the agent through the proxy when BEAN_PROXY_URL is set,
    with no gRPC dependency -- pure urllib against Connect HTTP/JSON."""

    @classmethod
    def setUpClass(cls):
        cls.proxy = HTTPServer(("127.0.0.1", 0), ProxyHandler)
        threading.Thread(target=cls.proxy.serve_forever, daemon=True).start()
        cls.proxy_url = f"http://127.0.0.1:{cls.proxy.server_port}"

    @classmethod
    def tearDownClass(cls):
        cls.proxy.shutdown()
        cls.proxy.server_close()

    def _sandbox(self, domain="sandbox.local"):
        from bean import Sandbox
        client = BeanClient(api_key="k", base_url="http://127.0.0.1:1",
                            proxy_url=self.proxy_url)
        return Sandbox(id="sbx_dp1", state="RUNNING", image="x",
                       domain=domain, _client=client)

    def test_exec_goes_through_the_proxy(self):
        sb = self._sandbox()
        r = sb.exec(["echo", "viaproxy"])
        self.assertEqual(r.exit_code, 0)
        self.assertEqual(r.stdout, "echo viaproxy")
        self.assertEqual(r.duration_ms, 7)
        # Addressed to the agent port of this sandbox under its domain.
        self.assertEqual(ProxyHandler.last_host, "10001-sbx_dp1.sandbox.local")

    def test_read_file_through_the_proxy(self):
        sb = self._sandbox()
        self.assertEqual(sb.read_file("/x"), b"proxied-bytes")

    def test_write_file_through_the_proxy(self):
        sb = self._sandbox()
        self.assertEqual(sb.write_file("/x", b"hello world"), 11)

    def test_no_proxy_keeps_the_relay(self):
        # Without a proxy URL the data plane is off: _dataplane_for returns None.
        client = BeanClient(api_key="k", base_url="http://127.0.0.1:1")
        self.assertIsNone(client._dataplane_for("sbx", ""))

    def test_authority_without_domain_is_bare_label(self):
        sb = self._sandbox(domain="")
        sb.exec(["echo", "hi"])
        self.assertEqual(ProxyHandler.last_host, "10001-sbx_dp1")


if __name__ == "__main__":
    unittest.main()
