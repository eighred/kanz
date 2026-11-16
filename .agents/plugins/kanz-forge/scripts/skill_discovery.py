"""Bounded read-only Codex CLI skill discovery for disposable install verification."""

import json
import queue
import subprocess
import threading
import time


def discover_skills(cli, root, env=None, timeout=30):
    process = subprocess.Popen([cli, "app-server", "--stdio"], cwd=root, env=env,
                               stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=subprocess.DEVNULL, text=True, encoding="utf-8")
    messages = queue.Queue()

    def consume():
        try:
            for line in process.stdout:
                try:
                    message = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if isinstance(message, dict):
                    messages.put(message)
        finally:
            messages.put(None)

    reader = threading.Thread(target=consume, daemon=True)
    reader.start()

    def send(message):
        process.stdin.write(json.dumps(message) + "\n")
        process.stdin.flush()

    def receive(identifier, deadline):
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("Codex skill discovery timed out")
            try:
                message = messages.get(timeout=remaining)
            except queue.Empty as exc:
                raise TimeoutError("Codex skill discovery timed out") from exc
            if message is None:
                raise ValueError("Codex skill discovery ended before its response")
            if message.get("id") == identifier:
                if "error" in message:
                    raise ValueError("Codex skill discovery returned an error")
                return message["result"]

    deadline = time.monotonic() + timeout
    try:
        send({"id": 1, "method": "initialize", "params": {"clientInfo": {"name": "forge_verification", "version": "1"}}})
        receive(1, deadline)
        send({"method": "initialized", "params": {}})
        send({"id": 2, "method": "skills/list", "params": {"cwds": [str(root)], "forceReload": True}})
        result = receive(2, deadline)
        groups = result.get("data") if isinstance(result, dict) else None
        if not isinstance(groups, list) or len(groups) != 1:
            raise ValueError("Codex skill discovery returned an incomplete workspace result")
        group = groups[0]
        if not isinstance(group, dict) or group.get("errors"):
            raise ValueError("Codex skill discovery reported workspace errors")
        skills = group.get("skills")
        if not isinstance(skills, list) or any(
            not isinstance(skill, dict) or not isinstance(skill.get("name"), str)
            or not isinstance(skill.get("path"), str) or not isinstance(skill.get("enabled"), bool)
            for skill in skills
        ):
            raise ValueError("Codex skill discovery returned malformed skills")
        return skills
    finally:
        if process.stdin and not process.stdin.closed:
            try:
                process.stdin.close()
            except OSError:
                pass
        try:
            process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            process.terminate()
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=3)
        finally:
            process.stdout.close()
            reader.join(timeout=1)
