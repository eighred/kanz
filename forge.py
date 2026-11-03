"""Repository-root entry point; all behavior belongs to the tracked Forge plugin."""
from pathlib import Path
import runpy
import sys

scripts = Path(__file__).resolve().parent / ".agents/plugins/kanz-forge/scripts"
sys.path.insert(0, str(scripts))
runpy.run_path(str(scripts / "command.py"), run_name="__main__")
