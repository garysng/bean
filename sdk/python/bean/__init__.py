"""bean SDK: minimal synchronous client for the bean sandbox platform."""

from __future__ import annotations

import base64
import io
import json
import os
import tarfile
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
    # None means the server did not report it, which is how a snapshot taken
    # before the field existed reads. Those all carried memory, so None is
    # closer to True than to False — but it is kept distinct so callers can tell
    # "unknown" from "confirmed no memory".
    include_memory: Optional[bool] = None
    # base_id is what the snapshot was actually captured against, which is not
    # always what was asked for: a chain past its depth limit is answered with a
    # full snapshot and an empty base_id.
    base_id: str = ""
    chain_depth: int = 0
    _client: "BeanClient" = field(default=None, repr=False, compare=False)

    @classmethod
    def _from_json(cls, obj: Dict[str, Any], client: "BeanClient" = None) -> "Snapshot":
        return cls(
            id=obj.get("id", ""), state=obj.get("state", ""),
            sandbox_id=obj.get("sandboxId", ""), image=obj.get("image", ""),
            name=obj.get("name", ""), size_bytes=obj.get("sizeBytes", 0),
            labels=obj.get("labels") or {},
            include_memory=obj.get("includeMemory"),
            base_id=obj.get("baseId", ""), chain_depth=obj.get("chainDepth", 0),
            _client=client,
        )

    @property
    def resumes_guest(self) -> bool:
        """Whether restoring this snapshot resumes the guest or boots it fresh.

        An unreported include_memory reads as True: every snapshot from before
        that field existed captured memory, and treating those as memoryless
        would claim a restore reboots when it actually resumes.
        """
        return self.include_memory is None or self.include_memory

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
        include_memory: bool = True,
        base: str = "",
    ) -> Snapshot:
        """Capture this sandbox so it can be restored later.

        The sandbox keeps running unless keep_running is False.

        include_memory=False captures only the filesystem. The restore boots a
        fresh guest instead of resuming this one, but it can land on any CPU:
        guest memory records what the CPU it booted on offered, and a snapshot
        carrying it can only be restored on a compatible vendor and family.

        base names an earlier snapshot to capture against, so this one holds
        only the memory written since. It needs include_memory, and the node
        must have been started with dirty-page tracking on. The returned
        snapshot's base_id says what was actually produced: a chain past its
        depth limit is answered with a full snapshot and an empty base_id.
        """
        body = {
            "name": name,
            "labels": labels or {},
            "keepRunning": keep_running,
            "includeMemory": include_memory,
        }
        if base:
            body["base"] = base
        data = self._client._request("POST", f"/v1/sandboxes/{self.id}/snapshot", body)
        if not keep_running:
            self.state = "STOPPED"
        return Snapshot._from_json(data["snapshot"], self._client)

    def commit(self, tag: str) -> str:
        """Freeze this sandbox's filesystem as a reusable base image.

        Returns the image reference, which any sandbox can then start from.

        This is not a snapshot. A snapshot carries memory and device state and
        restores only on the runtime tier that produced it, so it recovers this
        one sandbox. A committed image is just a filesystem, usable as anyone's
        base — which is what sharing a prepared environment needs.

        The sandbox keeps running. The tag must not already exist: images are
        immutable, so a new version needs a new tag.
        """
        data = self._client._request(
            "POST", f"/v1/sandboxes/{self.id}/commit", {"tag": tag}
        )
        return data["imageRef"]

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

    def build(
        self,
        tag: str,
        dockerfile: str,
        context_dir: Optional[str] = None,
        build_args: Optional[Dict[str, str]] = None,
        size_mib: int = 0,
    ) -> Dict[str, Any]:
        """Build an image from a Dockerfile on the platform.

        Returns immediately with the image reference and the node building it;
        a build takes minutes, so follow it with status(tag) until the state is
        READY or FAILED.

        dockerfile is the file's content, not a path. context_dir is packed and
        uploaded for COPY and ADD, so no local Docker is needed and the build
        cache is shared across the cluster rather than living on one machine.
        """
        body: Dict[str, Any] = {"tag": tag, "dockerfile": dockerfile}
        if build_args:
            body["buildArgs"] = build_args
        if size_mib:
            body["sizeMiB"] = size_mib
        if context_dir:
            body["contextTar"] = base64.b64encode(
                _pack_context(context_dir)
            ).decode("ascii")
        return self._client._request("POST", "/v1/images/build", body)


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


def _pack_context(directory: str) -> bytes:
    """Tar a build context for upload.

    .git is always excluded: it is large, never needed by a build, and shipping
    it would put the repository's history into the context.
    """
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as tf:
        for root, dirs, files in os.walk(directory):
            dirs[:] = [d for d in dirs if d != ".git"]
            for name in files:
                full = os.path.join(root, name)
                rel = os.path.relpath(full, directory)
                # The Dockerfile travels inline, so it is not packed here.
                if rel == "Dockerfile":
                    continue
                tf.add(full, arcname=rel)
    return buf.getvalue()
