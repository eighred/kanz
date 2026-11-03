"""Find review candidates in tracked Markdown at one immutable Git revision."""
import io
from pathlib import Path
import posixpath
import re
import subprocess
from urllib.parse import unquote, urlsplit
import zipfile


def missing(paths, documents):
    existing = set(paths)
    directories = {""}
    for path in existing:
        while "/" in path:
            path = path.rsplit("/", 1)[0]
            directories.add(path)
    results = []
    for source, body in documents:
        fence = None
        for number, line in enumerate(body.splitlines(), 1):
            delimiter = re.match(r"^\s{0,3}(`{3,}|~{3,})", line)
            if delimiter:
                mark = delimiter[1]
                if fence is None:
                    fence = mark
                elif mark.startswith(fence):
                    fence = None
                continue
            if fence:
                continue
            for link in re.finditer(r"\]\((?:<([^>]+)>|([^()\s]+))\)", line):
                raw = link[1] or link[2]
                parsed = urlsplit(raw)
                if parsed.scheme or parsed.netloc or not parsed.path or parsed.path.startswith("/"):
                    continue
                target = posixpath.normpath(posixpath.join(posixpath.dirname(source), unquote(parsed.path)))
                if target == ".." or target.startswith("../"):
                    continue
                if target not in existing and target not in directories:
                    results.append({"source": source, "line": number, "missing": target})
    return results


def scan(root):
    def git(*args):
        return subprocess.check_output(["git","-C",str(root),*args],timeout=60)
    sha = git("rev-parse","HEAD").decode().strip()
    paths = git("ls-tree","-rz","--name-only",sha).decode().split("\0")
    with zipfile.ZipFile(io.BytesIO(git("archive","--format=zip",sha,"--","*.md"))) as archive:
        docs = ((p, archive.read(p).decode("utf-8-sig",errors="replace"))
                for p in archive.namelist() if p.endswith(".md"))
        return {"revision":sha,"scope":"relative inline links in tracked Markdown; candidates require review",
                "candidates":missing(paths,docs)}
