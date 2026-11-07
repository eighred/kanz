"""Closed operator entrypoints loaded from checked source, never repository pyc."""
import sys
from pathlib import Path
import types


def launch(root, entry, arguments):
    if not sys.flags.isolated:
        raise ValueError("isolated Python required")
    recovery = ("bounded_process", "policy_state", "policy_client", "rollout_scope", "recovery_receipts")
    dependencies = {
        "preflight": ("bounded_process",),
        "recovery_host": ("bounded_process", "policy_state", "transport_codec", "recovery_receipts", "recovery_provenance"),
        "recovery_plan": recovery,
        "recovery_restore": (*recovery, "recovery_plan"),
        "image_identity": ("policy_state", "transaction", "rollout_scope", "rollout_probe"),
        "rollout_engine": ("bounded_process", "policy_state", "transaction", "policy_client", "rollout_scope", "rollout_probe"),
    }
    if entry not in dependencies:
        raise ValueError("unapproved source entrypoint")
    root = Path(root).resolve(strict=True)
    # -I excludes script/cwd and PYTHONPATH. Never add repository paths here.
    for name in (*dependencies[entry], entry):
        path = root / (name + ".py")
        if path.is_symlink() or not path.is_file() or path.stat().st_size > 65536:
            raise ValueError("invalid checked source")
        with path.open("rb") as stream:
            source = stream.read(65537)
        if len(source) > 65536:
            raise ValueError("checked source exceeds bound")
        module_name = "__main__" if name == entry else name
        module = types.ModuleType(module_name)
        module.__file__ = str(path)
        sys.modules[module_name] = module
        if name == entry:
            sys.argv = [str(path), *arguments]
        exec(compile(source, str(path), "exec"), module.__dict__)


if __name__ == "__main__":
    launch(sys.argv[1], sys.argv[2], sys.argv[3:])
