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
        for line in process.stdout:
            try:
                messages.put(json.loads(line))
            except json.JSONDecodeError:
                continue

    threading.Thread(target=consume, daemon=True).start()

    def send(message):
        process.stdin.write(json.dumps(message) + "\n")
        process.stdin.flush()

    def receive(identifier, deadline):
        while True:
            try:
                message = messages.get(timeout=max(0, deadline - time.monotonic()))
            except queue.Empty as exc:
                raise TimeoutError("Codex skill discovery timed out") from exc
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
        return [skill for group in result["data"] for skill in group["skills"]]
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
            process.wait(timeout=3)
