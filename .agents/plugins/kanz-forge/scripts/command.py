"""Launch a Kanz Forge action in interactive Codex using the current permissions."""
import argparse
import json
import sqlite3
import shutil
import sys
import evidence
import subprocess
from pathlib import Path


def main():
    skill = Path(__file__).resolve().parents[1] / "skills" / "commands"
    catalog = json.loads((skill / "commands.json").read_text(encoding="utf-8"))
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--preview", action="store_true", help="print without launching Codex")
    parser.add_argument("action", nargs="?", default="/help", choices=["/help", *catalog])
    parser.add_argument("target", nargs="*")
    args = parser.parse_args()
    if args.action == "/help":
        if args.target not in ([], ["all"]):
            parser.error("use /help or /help all")
        for name, entry in catalog.items():
            if not args.target and not entry.get("primary"):
                continue
            print(f"{name:8} {entry['description']}")
        if not args.target:
            print("/help all   Show specialist commands")
        return 0
    target = " ".join(args.target).strip()
    entry = catalog[args.action]
    if not target and entry["target"] == "required":
        parser.error(f"{args.action} requires an issue, PR, run, or description")
    if entry.get("mode") == "evidence":
        if args.preview:
            print(entry["description"], target)
            return 0
        try:
            print(json.dumps(evidence.run(args.action, target), indent=2))
            return 0
        except (sqlite3.Error, OSError, ValueError, KeyError, subprocess.SubprocessError) as exc:
            print(str(exc), file=sys.stderr)
            return 1
    prompt = (f"$kanz-forge:commands {args.action}\n{entry['workflow']}\n"
              f"User target: {json.dumps(target, ensure_ascii=False)}")
    if args.preview:
        print(prompt)
        return 0
    # Avoid invoking a shell or interpreting user arguments as client options.
    exe = shutil.which("codex.exe") or shutil.which("codex")
    if not exe or Path(exe).suffix.lower() in {".cmd", ".bat"}:
        parser.error("A native Codex CLI is required; use --preview for chat instead")
    return subprocess.call([exe, prompt])


if __name__ == "__main__":
    raise SystemExit(main())
