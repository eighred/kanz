"""Check every effective-source README and root contributor guide, offline."""
import argparse
import json
from pathlib import Path
import re
import subprocess
from urllib.parse import unquote, urlsplit


def source_files(root):
    raw = subprocess.check_output(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"],
        cwd=root, timeout=30)
    return sorted({p.decode("utf-8") for p in raw.split(b"\0") if p})


def prose_lines(text):
    fence = None
    for number, line in enumerate(text.splitlines(), 1):
        marker = re.match(r"^\s{0,3}(`{3,}|~{3,})", line)
        if marker:
            value = marker.group(1)
            if fence is None:
                fence = value
            elif value[0] == fence[0] and len(value) >= len(fence):
                fence = None
            continue
        if fence is None:
            yield number, line


def anchors(text):
    result, occurrences = set(), {}
    for _, line in prose_lines(text):
        result.update(re.findall(r'<a\s+(?:id|name)=["\']([^"\']+)', line))
        heading = re.match(r"^#{1,6}\s+(.+?)(?:\s+#+)?$", line)
        if not heading:
            continue
        title = re.sub(r"\[([^\]]+)\]\([^)]+\)", r"\1", heading.group(1))
        slug = re.sub(r"[^\w\- ]", "", title.lower()).replace(" ", "-")
        occurrence = occurrences.get(slug, 0)
        occurrences[slug] = occurrence + 1
        result.add(slug + (f"-{occurrence}" if occurrence else ""))
    return result


def check_document(root, relative):
    document = root / relative
    text = document.read_text(encoding="utf-8-sig")
    issues, count = [], 0
    lines = list(prose_lines(text))
    definitions = {}
    for _, line in lines:
        definition = re.match(r"^\s{0,3}\[([^\]]+)\]:\s*(\S+)", line)
        if definition:
            definitions[definition.group(1).casefold()] = definition.group(2)
    targets = []
    for number, line in lines:
        targets.extend((number, match.group(1)) for match in
                       re.finditer(r"!?\[[^\]]*\]\((<[^>]+>|[^)\s]+)(?:\s+\"[^\"]*\")?\)", line))
        for match in re.finditer(r"!?\[([^\]]+)\]\[([^\]]*)\]", line):
            key = (match.group(2) or match.group(1)).casefold()
            if key in definitions:
                targets.append((number, definitions[key]))
            else:
                issues.append(f"{relative}:{number}: undefined link reference {key}")
    for number, target in targets:
        target = target.strip("<>")
        parsed = urlsplit(target)
        if parsed.scheme or parsed.netloc:
            continue
        count += 1
        destination = (document.parent / unquote(parsed.path)).resolve() if parsed.path else document
        if not destination.is_relative_to(root.resolve()):
            issues.append(f"{relative}:{number}: link escapes repository: {target}")
        elif not destination.exists():
            issues.append(f"{relative}:{number}: missing local target: {target}")
        elif parsed.fragment and destination.is_file() and destination.suffix.lower() == ".md":
            if unquote(parsed.fragment) not in anchors(destination.read_text(encoding="utf-8-sig")):
                issues.append(f"{relative}:{number}: missing heading: {target}")
    if not any(re.match(r"^#\s+\S", line) for _, line in lines):
        issues.append(f"{relative}: missing document title")
    return count, issues


def check(root, files):
    readmes = [p for p in files if Path(p).name == "README.md" and (root / p).is_file()]
    guides = [p for p in ("AGENTS.md", "CONTRIBUTING.md") if (root / p).is_file()]
    links, issues = 0, []
    for relative in sorted(set(readmes + guides)):
        count, failures = check_document(root, relative)
        links += count
        issues.extend(failures)
    if not readmes:
        issues.append("README inventory is empty")
    return {"readmes": readmes, "guides": guides, "local_links": links, "issues": issues}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inventory", action="store_true", help="include every reviewed path")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    result = check(root, source_files(root))
    if not args.inventory:
        result["readmes"] = len(result["readmes"])
    print(json.dumps(result, indent=2))
    return bool(result["issues"])


if __name__ == "__main__":
    raise SystemExit(main())
