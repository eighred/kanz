#!/usr/bin/env python3
"""Validate the checkout-owned Kanz Forge plugin without external packages."""

import argparse
import json
import re
import subprocess
import sys
from pathlib import Path

PLUGIN = Path(__file__).resolve().parents[1]
ROOT = PLUGIN.parents[2]
MARKETPLACE = ROOT / ".agents/plugins/marketplace.json"
MANIFEST = PLUGIN / ".codex-plugin/plugin.json"
EXPECTED_SKILLS = {
    "commands",
    "engineering-standard",
    "financial-verification",
    "security-audit",
    "issue-archaeology",
    "kanz-verify",
    "pr-delivery",
    "stack-routing",
}


def fail(message: str) -> None:
    raise ValueError(message)


def local_path(base: Path, value: str) -> Path:
    if not value.startswith("./") or ".." in Path(value).parts:
        fail(f"unsafe plugin path: {value!r}")
    resolved = (base / value).resolve()
    if not resolved.is_relative_to(ROOT):
        fail(f"plugin path escapes checkout: {value!r}")
    return resolved


def frontmatter(skill: Path) -> dict[str, str]:
    lines = skill.read_text(encoding="utf-8-sig").splitlines()
    if not lines or lines[0] != "---":
        fail(f"missing frontmatter: {skill}")
    try:
        end = lines.index("---", 1)
    except ValueError:
        fail(f"unterminated frontmatter: {skill}")
    fields = {}
    for line in lines[1:end]:
        if ":" in line:
            key, value = line.split(":", 1)
            fields[key.strip()] = value.strip().strip('"\'')
    if not fields.get("name") or not fields.get("description"):
        fail(f"name/description required: {skill}")
    if len(fields["description"]) > 180 or "[TODO" in skill.read_text(encoding="utf-8-sig"):
        fail(f"description too broad or unfinished scaffold: {skill}")
    return fields


def referenced_files(skill: Path) -> list[Path]:
    text = skill.read_text(encoding="utf-8-sig")
    references = []
    for target in re.findall(r"\[[^\]]+\]\(([^)]+)\)", text):
        if target.startswith(("https://", "http://", "#")):
            continue
        path = (skill.parent / target).resolve()
        if not path.is_relative_to(PLUGIN) or not path.is_file():
            fail(f"missing or escaping skill reference {target!r} in {skill}")
        references.append(path)
    return references


def tracked(paths: list[Path]) -> None:
    required = {path.relative_to(ROOT).as_posix() for path in paths}
    raw = subprocess.check_output(["git", "ls-files", "-z", "--", ".agents/plugins"], cwd=ROOT)
    missing = required - set(raw.decode("utf-8").split("\0"))
    if missing:
        fail("required plugin files not tracked: " + ", ".join(sorted(missing)))


def validate(check_tracked: bool) -> None:
    marketplace = json.loads(MARKETPLACE.read_text(encoding="utf-8-sig"))
    manifest = json.loads(MANIFEST.read_text(encoding="utf-8-sig"))
    if not re.fullmatch(r"[A-Za-z0-9_-]+", marketplace.get("name", "")):
        fail("marketplace name is invalid")
    entries = [p for p in marketplace.get("plugins", []) if p.get("name") == "kanz-forge"]
    if len(entries) != 1:
        fail("marketplace must contain exactly one kanz-forge entry")
    entry = entries[0]
    source = entry.get("source", {})
    if source.get("source") != "local" or local_path(ROOT, source.get("path", "")) != PLUGIN:
        fail("marketplace source does not resolve to the repository-owned plugin")
    if entry.get("policy") != {"installation": "AVAILABLE", "authentication": "ON_INSTALL"}:
        fail("marketplace installation/authentication policy is incomplete")
    if entry.get("category") != "Productivity":
        fail("marketplace category is missing")
    if manifest.get("name") != "kanz-forge" or not manifest.get("version"):
        fail("plugin name/version is invalid")
    if local_path(PLUGIN, manifest.get("skills", "")) != PLUGIN / "skills":
        fail("manifest skills directory is incorrect")
    for field in ("displayName", "shortDescription", "longDescription", "developerName"):
        if not manifest.get("interface", {}).get(field):
            fail(f"manifest interface.{field} is missing")
    if (ROOT / "plugins/kanz-forge").exists():
        fail("duplicate root plugin would compete with .agents authority")
    skills = sorted((PLUGIN / "skills").glob("*/SKILL.md"))
    names = {path.parent.name for path in skills}
    if names != EXPECTED_SKILLS:
        fail(f"skill inventory differs: {sorted(names ^ EXPECTED_SKILLS)}")
    files = [MARKETPLACE, MANIFEST, Path(__file__).resolve()]
    for skill in skills:
        if frontmatter(skill)["name"] != skill.parent.name:
            fail(f"skill name does not match its directory: {skill}")
        files.append(skill)
        files.extend(referenced_files(skill))
    agents = (ROOT / "AGENTS.md").read_text(encoding="utf-8-sig")
    if ".agents/plugins/kanz-forge/" not in agents:
        fail("AGENTS.md does not point to the plugin authority")
    files.extend([PLUGIN / "upstreams.json", *(PLUGIN / "scripts").glob("*.py")])
    if check_tracked:
        tracked(files)
    print(f"kanz-forge: {len(skills)} skills, {len(files)} required files, marketplace={marketplace['name']}: OK")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tracked", action="store_true", help="require every referenced file in Git")
    args = parser.parse_args()
    try:
        validate(args.tracked)
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"kanz-forge: {exc}", file=sys.stderr)
        sys.exit(1)
