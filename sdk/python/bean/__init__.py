"""bean SDK: minimal synchronous client for the bean sandbox platform."""

from __future__ import annotations

import json
import os
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional

__all__ = ["BeanClient", "Sandbox", "ExecResult", "BeanAPIError"]


class BeanAPIError(Exception):
    def __init__(self, code: str, message: str, http_status: int):
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message
        self.http_status = http_status


@dataclass
class ExecResult:
    exit_code: int
    stdout: str
    stderr: str
    truncated: bool
    duration_ms: int


@dataclass
class Sandbox:
    id: str
    state: str
    image: str
    labels: Dict[str, str] = field(default_factory=dict)
    _client: "BeanClient" = field(default=None, repr=False, compare=False)

    def exec(
        self,
        cmd,
        cwd: str = "",
        env: Optional[Dict[str, str]] = None,
        timeout: int = 0,
    ) -> ExecResult:
        if isinstance(cmd, str):
            cmd = ["/bin/sh", "-c", cmd]
        body = {"cmd": cmd, "cwd": cwd, "env": env or {}, "timeoutSeconds": timeout}
        data = self._client._request("POST", f"/v1/sandboxes/{self.id}/exec", body)
        return ExecResult(
            exit_code=data["exitCode"],
            stdout=data["stdout"],
            stderr=data["stderr"],
            truncated=data["truncated"],
            duration_ms=data["durationMs"],
        )

    def write_file(self, path: str, content, mkdirs: bool = True) -> int:
        if isinstance(content, str):
            content = content.encode()
        q = urllib.parse.urlencode({"path": path, "mkdirs": "true" if mkdirs else "false"})
        data = self._client._request_raw(
            "PUT", f"/v1/sandboxes/{self.id}/files?{q}", content
        )
        return json.loads(data)["bytesWritten"]

    def read_file(self, path: str) -> bytes:
        q = urllib.parse.urlencode({"path": path})
        return self._client._request_raw("GET", f"/v1/sandboxes/{self.id}/files?{q}")

    def ls(self, path: str = "/") -> List[Dict[str, Any]]:
        q = urllib.parse.urlencode({"path": path})
        return self._client._request("GET", f"/v1/sandboxes/{self.id}/files/ls?{q}")["entries"]

    def pause(self) -> None:
        self._client._request("POST", f"/v1/sandboxes/{self.id}/pause")
        self.state = "PAUSED"

    def resume(self) -> None:
        self._client._request("POST", f"/v1/sandboxes/{self.id}/resume")
        self.state = "RUNNING"

    def events(self) -> List[Dict[str, Any]]:
        return self._client._request("GET", f"/v1/sandboxes/{self.id}/events")["events"]

    def refresh(self) -> "Sandbox":
        data = self._client._request("GET", f"/v1/sandboxes/{self.id}")["sandbox"]
        self.state = data["state"]
        return self

    def kill(self, force: bool = False) -> None:
        path = f"/v1/sandboxes/{self.id}"
        if force:
            path += "?force=true"
        self._client._request("DELETE", path)
        self.state = "STOPPED"

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *exc) -> None:
        try:
            self.kill(force=True)
        except BeanAPIError:
            pass


class _Sandboxes:
    def __init__(self, client: "BeanClient"):
        self._client = client

    def create(
        self,
        image: str,
        cpu: float = 1,
        memory_mib: int = 512,
        disk_mib: int = 20480,
        env: Optional[Dict[str, str]] = None,
        cmd: Optional[List[str]] = None,
        auto_start_cmd: bool = False,
        labels: Optional[Dict[str, str]] = None,
        idle_timeout: Optional[str] = None,
        on_idle: str = "pause",
    ) -> Sandbox:
        body: Dict[str, Any] = {
            "image": image,
            "resources": {"cpu": cpu, "memoryMiB": memory_mib, "diskMiB": disk_mib},
            "env": env or {},
            "labels": labels or {},
            "autoStartCmd": auto_start_cmd,
        }
        if cmd:
            body["cmd"] = cmd
        if idle_timeout is not None:
            body["lifecycle"] = {"idleTimeout": idle_timeout, "onIdle": on_idle}
        data = self._client._request("POST", "/v1/sandboxes", body)["sandbox"]
        return Sandbox(
            id=data["id"], state=data["state"], image=data["image"],
            labels=data.get("labels") or {}, _client=self._client,
        )

    def get(self, sandbox_id: str) -> Sandbox:
        data = self._client._request("GET", f"/v1/sandboxes/{sandbox_id}")["sandbox"]
        return Sandbox(
            id=data["id"], state=data["state"], image=data["image"],
            labels=data.get("labels") or {}, _client=self._client,
        )

    def list(self, labels: Optional[Dict[str, str]] = None) -> List[Sandbox]:
        path = "/v1/sandboxes"
        if labels:
            k, v = next(iter(labels.items()))
            path += "?label=" + urllib.parse.quote(f"{k}={v}")
        data = self._client._request("GET", path)["sandboxes"]
        return [
            Sandbox(id=d["id"], state=d["state"], image=d["image"],
                    labels=d.get("labels") or {}, _client=self._client)
            for d in data
        ]


class BeanClient:
    def __init__(self, api_key: Optional[str] = None, base_url: Optional[str] = None):
        self.api_key = api_key or os.environ.get("BEAN_API_KEY", "")
        self.base_url = (base_url or os.environ.get("BEAN_BASE_URL", "http://127.0.0.1:8080")).rstrip("/")
        self.sandboxes = _Sandboxes(self)

    def _request_raw(self, method: str, path: str, body: Optional[bytes] = None) -> bytes:
        req = urllib.request.Request(self.base_url + path, data=body, method=method)
        req.add_header("Authorization", f"Bearer {self.api_key}")
        try:
            with urllib.request.urlopen(req, timeout=900) as resp:
                return resp.read()
        except urllib.error.HTTPError as e:
            raw = e.read()
            try:
                err = json.loads(raw)["error"]
                raise BeanAPIError(err["code"], err["message"], e.code) from None
            except (KeyError, ValueError):
                raise BeanAPIError("HTTP_ERROR", raw.decode(errors="replace"), e.code) from None

    def _request(self, method: str, path: str, body: Optional[dict] = None) -> Any:
        payload = json.dumps(body).encode() if body is not None else None
        data = self._request_raw(method, path, payload)
        if not data:
            return {}
        return json.loads(data)
