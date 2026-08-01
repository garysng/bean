"""bean SDK: minimal synchronous client for the bean sandbox platform."""

from __future__ import annotations

import json
import os
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional

__all__ = [
    "BeanClient", "Sandbox", "Snapshot", "ExecResult", "Event",
    "BeanAPIError", "BeanConnectionError",
]


class BeanAPIError(Exception):
    def __init__(self, code: str, message: str, http_status: int = 0):
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message
        self.http_status = http_status


class BeanConnectionError(BeanAPIError):
    """Transport-level failure (connection refused, DNS, timeout)."""

    def __init__(self, message: str):
        super().__init__("CONNECTION_ERROR", message, 0)


@dataclass
class ExecResult:
    exit_code: int
    stdout: str
    stderr: str
    truncated: bool
    duration_ms: int


@dataclass
class Snapshot:
    """A captured sandbox that can be restored later."""

    id: str
    state: str
    sandbox_id: str
    image: str = ""
    name: str = ""
    size_bytes: int = 0
    labels: Dict[str, str] = field(default_factory=dict)
    _client: "BeanClient" = field(default=None, repr=False, compare=False)

    @classmethod
    def _from_json(cls, obj: Dict[str, Any], client: "BeanClient" = None) -> "Snapshot":
        return cls(
            id=obj.get("id", ""), state=obj.get("state", ""),
            sandbox_id=obj.get("sandboxId", ""), image=obj.get("image", ""),
            name=obj.get("name", ""), size_bytes=obj.get("sizeBytes", 0),
            labels=obj.get("labels") or {}, _client=client,
        )

    def delete(self) -> None:
        self._client._request("DELETE", f"/v1/snapshots/{self.id}")


@dataclass
class Event:
    type: str
    sandbox_id: str
    timestamp: str
    data: Dict[str, str] = field(default_factory=dict)
    version: str = "v1"

    @classmethod
    def _from_json(cls, obj: Dict[str, Any]) -> "Event":
        return cls(
            type=obj.get("type", ""),
            sandbox_id=obj.get("sandboxId", ""),
            timestamp=obj.get("timestamp", ""),
            data=obj.get("data") or {},
            version=obj.get("version", "v1"),
        )


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

    def snapshot(
        self,
        name: str = "",
        labels: Optional[Dict[str, str]] = None,
        keep_running: bool = True,
    ) -> Snapshot:
        """Capture this sandbox so it can be restored later.

        The sandbox keeps running unless keep_running is False.
        """
        body = {"name": name, "labels": labels or {}, "keepRunning": keep_running}
        data = self._client._request("POST", f"/v1/sandboxes/{self.id}/snapshot", body)
        if not keep_running:
            self.state = "STOPPED"
        return Snapshot._from_json(data["snapshot"], self._client)

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
        image: str = "",
        cpu: float = 1,
        memory_mib: int = 512,
        disk_mib: int = 20480,
        env: Optional[Dict[str, str]] = None,
        cmd: Optional[List[str]] = None,
        auto_start_cmd: bool = False,
        labels: Optional[Dict[str, str]] = None,
        idle_timeout: Optional[str] = None,
        on_idle: str = "pause",
        snapshot: str = "",
    ) -> Sandbox:
        """Create a sandbox from an image, or restore one from a snapshot.

        Exactly one of image or snapshot must be given.
        """
        if bool(image) == bool(snapshot):
            raise ValueError("provide exactly one of image or snapshot")
        body: Dict[str, Any] = {
            "resources": {"cpu": cpu, "memoryMiB": memory_mib, "diskMiB": disk_mib},
            "env": env or {},
            "labels": labels or {},
            "autoStartCmd": auto_start_cmd,
        }
        if image:
            body["image"] = image
        else:
            body["snapshot"] = snapshot
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


class _Snapshots:
    def __init__(self, client: "BeanClient"):
        self._client = client

    def list(self, labels: Optional[Dict[str, str]] = None) -> List[Snapshot]:
        path = "/v1/snapshots"
        if labels:
            k, v = next(iter(labels.items()))
            path += "?label=" + urllib.parse.quote(f"{k}={v}")
        data = self._client._request("GET", path)["snapshots"]
        return [Snapshot._from_json(d, self._client) for d in data]

    def get(self, snapshot_id: str) -> Snapshot:
        data = self._client._request("GET", f"/v1/snapshots/{snapshot_id}")["snapshot"]
        return Snapshot._from_json(data, self._client)

    def delete(self, snapshot_id: str) -> None:
        self._client._request("DELETE", f"/v1/snapshots/{snapshot_id}")


class _Images:
    def __init__(self, client: "BeanClient"):
        self._client = client

    def list(self) -> List[Dict[str, Any]]:
        return self._client._request("GET", "/v1/images")["images"] or []

    def status(self, ref: str) -> Dict[str, Any]:
        q = urllib.parse.urlencode({"ref": ref})
        return self._client._request("GET", f"/v1/images/status?{q}")

    def prewarm(
        self,
        refs: List[str],
        target_nodes: int = 0,
        region: str = "",
        priority: str = "",
    ) -> Dict[str, Any]:
        """Pull images onto nodes ahead of a batch."""
        body = {"refs": refs, "targetNodes": target_nodes,
                "region": region, "priority": priority}
        return self._client._request("POST", "/v1/images/prewarm", body)

    def prewarm_status(self, job_id: str) -> Dict[str, Any]:
        return self._client._request("GET", f"/v1/images/prewarm/{job_id}")


class _Events:
    def __init__(self, client: "BeanClient"):
        self._client = client

    def subscribe(
        self,
        sandbox_id: Optional[str] = None,
        labels: Optional[Dict[str, str]] = None,
        timeout: Optional[float] = None,
    ):
        """Yield Event objects from the server-sent event stream.

        Blocks until the connection drops or the caller stops iterating,
        so batch runs can react to lifecycle changes without polling.
        """
        params: Dict[str, str] = {}
        if sandbox_id:
            params["sandbox"] = sandbox_id
        if labels:
            k, v = next(iter(labels.items()))
            params["label"] = f"{k}={v}"
        path = "/v1/events"
        if params:
            path += "?" + urllib.parse.urlencode(params)

        req = urllib.request.Request(self._client.base_url + path, method="GET")
        req.add_header("Authorization", f"Bearer {self._client.api_key}")
        req.add_header("Accept", "text/event-stream")
        try:
            resp = urllib.request.urlopen(req, timeout=timeout)
        except urllib.error.HTTPError as e:
            raw = e.read()
            try:
                err = json.loads(raw)["error"]
                raise BeanAPIError(err["code"], err["message"], e.code) from None
            except (KeyError, ValueError):
                raise BeanAPIError("HTTP_ERROR", raw.decode(errors="replace"), e.code) from None
        except urllib.error.URLError as e:
            raise BeanConnectionError(f"{self._client.base_url}: {e.reason}") from None
        except (TimeoutError, OSError) as e:
            raise BeanConnectionError(f"{self._client.base_url}: {e}") from None

        with resp:
            for raw_line in resp:
                line = raw_line.decode("utf-8", errors="replace").rstrip("\r\n")
                # Comment lines (": connected", ": keepalive") and blank
                # separators carry no payload.
                if not line or line.startswith(":") or line.startswith("event:"):
                    continue
                if line.startswith("data:"):
                    payload = line[len("data:"):].strip()
                    if not payload:
                        continue
                    try:
                        yield Event._from_json(json.loads(payload))
                    except ValueError:
                        continue


class BeanClient:
    def __init__(
        self,
        api_key: Optional[str] = None,
        base_url: Optional[str] = None,
        timeout: float = 900.0,
    ):
        self.api_key = api_key or os.environ.get("BEAN_API_KEY", "")
        self.base_url = (base_url or os.environ.get("BEAN_BASE_URL", "http://127.0.0.1:8080")).rstrip("/")
        self.timeout = timeout
        self.sandboxes = _Sandboxes(self)
        self.snapshots = _Snapshots(self)
        self.images = _Images(self)
        self.events = _Events(self)

    def _request_raw(self, method: str, path: str, body: Optional[bytes] = None) -> bytes:
        req = urllib.request.Request(self.base_url + path, data=body, method=method)
        req.add_header("Authorization", f"Bearer {self.api_key}")
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                return resp.read()
        except urllib.error.HTTPError as e:
            # HTTPError subclasses URLError, so it must be handled first.
            raw = e.read()
            try:
                err = json.loads(raw)["error"]
                raise BeanAPIError(err["code"], err["message"], e.code) from None
            except (KeyError, ValueError):
                raise BeanAPIError("HTTP_ERROR", raw.decode(errors="replace"), e.code) from None
        except urllib.error.URLError as e:
            raise BeanConnectionError(f"{self.base_url}: {e.reason}") from None
        except (TimeoutError, OSError) as e:
            raise BeanConnectionError(f"{self.base_url}: {e}") from None

    def _request(self, method: str, path: str, body: Optional[dict] = None) -> Any:
        payload = json.dumps(body).encode() if body is not None else None
        data = self._request_raw(method, path, payload)
        if not data:
            return {}
        return json.loads(data)
