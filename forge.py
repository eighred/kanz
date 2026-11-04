"""Repository-root entry point; behavior belongs to the tracked Forge plugin."""
from pathlib import Path
import sys

# The implementation lives with the plugin; keep `python forge.py` as a CLI entry.
scripts = Path(__file__).resolve().parent / ".agents/plugins/kanz-forge/scripts"
sys.path.insert(0, str(scripts))
from command import main

if __name__ == "__main__":
    raise SystemExit(main())
