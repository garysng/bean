"""Data-plane client: sandbox operations straight to the agent through bean-proxy.

The agent is served over Connect (connectrpc), which speaks plain HTTP/JSON as
well as gRPC. That is what lets this SDK reach exec and file operations with only
`urllib` -- no grpcio, no generated stubs, keeping the SDK's zero-dependency
design. A unary call is a JSON POST to `/{service}/{method}`; the streaming file
RPCs use Connect's enveloped framing, which is a 5-byte prefix per message and
nothing more.

The client presents no per-sandbox token: the node's forwarder injects it. The
request carries only whatever the outer platform layer requires. Routing is by
Host header -- `{port}-{sandbox}.{domain}` -- so the proxy forwards to the agent
port (10001) of the right sandbox.
"""

from __future__ import annotations

import base64
import json
import struct
import urllib.error
import urllib.request
from typing import Any, Dict, Iterator, List, Optional

AGENT_PORT = 10001
_SERVICE = "bean.agent.v1.AgentService"


class DataPlaneError(Exception):
    """A data-plane call failed. Carries the Connect error code when present."""

    def __init__(self, code: str, message: str):
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message


def _authority(port: int, sandbox_id: str, domain: str) -> str:
    label = f"{port}-{sandbox_id}"
    return f"{label}.{domain}" if domain else label


def _encode_envelope(payload: bytes, end: bool = False) -> bytes:
    """Frame one Connect streaming message: flags byte + big-endian length."""
    flags = 0x02 if end else 0x00
    return struct.pack(">BI", flags, len(payload)) + payload


def _iter_envelopes(data: bytes) -> Iterator[tuple]:
    """Yield (flags, payload) for each enveloped message in a stream response."""
    off = 0
    while off + 5 <= len(data):
        flags, length = struct.unpack(">BI", data[off:off + 5])
        off += 5
        payload = data[off:off + length]
        off += length
        yield flags, payload


class DataPlane:
    """Reaches a sandbox's agent through bean-proxy over Connect HTTP/JSON.

    proxy_url is the proxy's address; domain is the sandbox's data-plane domain
    (from its record). Both come from the SDK, which resolves the domain from the
    sandbox before the first call.
    """

    def __init__(self, proxy_url: str, domain: str, timeout: float = 900.0):
        # Strip any scheme; requests are issued against the proxy with an explicit
        # Host header, so what matters is the proxy's own host:port.
        addr = proxy_url
        for sep in ("://",):
            i = addr.find(sep)
            if i >= 0:
                addr = addr[i + len(sep):]
        self.addr = addr.rstrip("/")
        self.domain = domain
        self.timeout = timeout

    def _post(self, method: str, sandbox_id: str, body: bytes,
              content_type: str) -> bytes:
        # The proxy routes on Host; the URL host is the proxy itself. Connect's
        # unary path is /{service}/{method}.
        host = _authority(AGENT_PORT, sandbox_id, self.domain)
        url = f"http://{self.addr}/{_SERVICE}/{method}"
        req = urllib.request.Request(url, data=body, method="POST")
        req.add_header("Host", host)
        req.add_header("Content-Type", content_type)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                return resp.read()
        except urllib.error.HTTPError as e:
            raw = e.read()
            try:
                obj = json.loads(raw)
                raise DataPlaneError(obj.get("code", "unknown"),
                                     obj.get("message", raw.decode(errors="replace"))) from None
            except ValueError:
                raise DataPlaneError("http_error", raw.decode(errors="replace")) from None
        except urllib.error.URLError as e:
            raise DataPlaneError("unavailable", f"{self.addr}: {e.reason}") from None

    def exec(self, sandbox_id: str, cmd: List[str], cwd: str = "",
             env: Optional[Dict[str, str]] = None, timeout_seconds: int = 0) -> Dict[str, Any]:
        """Unary Exec over Connect JSON. Returns the decoded response dict."""
        body = json.dumps({
            "sandboxId": sandbox_id, "cmd": cmd, "cwd": cwd,
            "env": env or {}, "timeoutSeconds": timeout_seconds,
        }).encode()
        raw = self._post("Exec", sandbox_id, body, "application/json")
        obj = json.loads(raw) if raw else {}
        # protobuf-JSON encodes bytes fields as base64 strings.
        return {
            "exitCode": obj.get("exitCode", 0),
            "stdout": base64.b64decode(obj["stdout"]) if obj.get("stdout") else b"",
            "stderr": base64.b64decode(obj["stderr"]) if obj.get("stderr") else b"",
            "truncated": obj.get("truncated", False),
            "durationMs": obj.get("durationMs", 0),
        }

    def read_file(self, sandbox_id: str, path: str) -> bytes:
        """ReadFile is a server stream; concatenate the enveloped chunks."""
        body = _encode_envelope(json.dumps({"sandboxId": sandbox_id, "path": path}).encode())
        raw = self._post("ReadFile", sandbox_id, body, "application/connect+json")
        out = bytearray()
        for flags, payload in _iter_envelopes(raw):
            if flags & 0x02:  # end-of-stream envelope: trailers/errors, not data
                trailer = json.loads(payload) if payload else {}
                if trailer.get("error"):
                    err = trailer["error"]
                    raise DataPlaneError(err.get("code", "unknown"), err.get("message", ""))
                continue
            chunk = json.loads(payload)
            if chunk.get("data"):
                out += base64.b64decode(chunk["data"])
        return bytes(out)

    def write_file(self, sandbox_id: str, path: str, content: bytes,
                   mkdirs: bool = True) -> int:
        """WriteFile is a client stream: a meta frame then a data frame."""
        meta = _encode_envelope(json.dumps({
            "meta": {"sandboxId": sandbox_id, "path": path, "mkdirs": mkdirs}
        }).encode())
        data = _encode_envelope(json.dumps({
            "data": base64.b64encode(content).decode()
        }).encode())
        raw = self._post("WriteFile", sandbox_id, meta + data, "application/connect+json")
        written = 0
        for flags, payload in _iter_envelopes(raw):
            obj = json.loads(payload) if payload else {}
            if flags & 0x02:
                if obj.get("error"):
                    raise DataPlaneError(obj["error"].get("code", "unknown"),
                                         obj["error"].get("message", ""))
                continue
            written = obj.get("bytesWritten", written)
        return written
