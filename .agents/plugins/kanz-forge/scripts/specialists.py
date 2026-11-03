"""Expose canonical Forge roles to Codex's project agent loader, without overwrites."""
import hashlib
import json
from pathlib import Path

PLUGIN = Path(__file__).resolve().parents[1]


def profiles(root, target, source=None):
    if target not in ("", "setup"):
        raise ValueError("use /agents or /agents setup")
    source = source or PLUGIN / "agents"
    local = root / ".codex" / "agents"
    if local.resolve() != root.resolve() / ".codex" / "agents":
        raise ValueError("agent directory redirects outside its expected location")
    states = []
    pending = []
    for path in sorted(source.glob("kanz_*.toml")):
        dst = local / path.name
        if dst.is_symlink() or dst.resolve() != local.resolve() / path.name:
            raise ValueError("refusing redirected agent profile")
        data = path.read_bytes()
        state = "missing"
        if dst.exists():
            state = "ready" if dst.read_bytes().replace(b"\r\n", b"\n") == data.replace(b"\r\n", b"\n") else "conflict"
        states.append({"agent":path.stem, "state":state, "source_sha256":hashlib.sha256(data).hexdigest()})
        if state == "missing":
            pending.append((dst,data))
    if not states:
        raise ValueError("no Forge specialist profiles found")
    if target:
        if any(s["state"] == "conflict" for s in states):
            raise ValueError("different project agent exists; inspect /agents before setup")
        local.mkdir(parents=True,exist_ok=True)
        for dst,data in pending:
            with dst.open("xb") as out:
                out.write(data)
        return profiles(root,"",source)
    return {"project_agent_directory":str(local), "profiles":states,
            "scope":"project configuration on disk; actual delegation requires a supporting host"}
