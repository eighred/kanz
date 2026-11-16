"""Lossless, bounded XZ transport for the fixed operator source bundle."""
import base64
import lzma
import hashlib
import json
import ast

if not lzma.is_check_supported(lzma.CHECK_CRC64):
    raise ValueError("XZ CRC64 support required")

ENCODED_LIMIT = 20000
EXPANDED_LIMIT = 524288
MEMORY_LIMIT = 16777216

# One literal partition for the checked Python and PowerShell source collectors.
# Transport placement never changes the closed loader's module execution order.
SOURCE_BUNDLES = {"normal":{"files":["policy_state.py","transaction.py","policy_client.py","bounded_process.py","rollout_scope.py","rollout_engine.py","network_observation.py"],"fixed":["source_loader.py","rollout_probe.py"]},"plan":{"files":["policy_state.py","policy_client.py","rollout_scope.py","recovery_receipts.py","recovery_plan.py","bounded_process.py"],"fixed":["source_loader.py"]},"restore":{"files":["policy_state.py","policy_client.py","rollout_scope.py","recovery_receipts.py","recovery_plan.py","recovery_restore.py"],"fixed":["source_loader.py","bounded_process.py"]}}


def encode(raw):
    if not isinstance(raw, bytes) or len(raw) > EXPANDED_LIMIT:
        raise ValueError("bundle bound")
    packed = lzma.compress(raw, format=lzma.FORMAT_XZ, check=lzma.CHECK_CRC64,
                          filters=[{"id": lzma.FILTER_LZMA2, "preset": 6, "dict_size": 1048576,
                                    "pb": 0, "lc": 3, "nice_len": 80, "depth": 64}])
    result = base64.b64encode(packed).decode("ascii")
    if len(result) > ENCODED_LIMIT:
        raise ValueError("encoded bundle bound")
    return result


def decode(encoded):
    if not isinstance(encoded, str) or len(encoded) > ENCODED_LIMIT:
        raise ValueError("encoded bundle bound")
    packed = base64.b64decode(encoded, validate=True)
    decoder = lzma.LZMADecompressor(format=lzma.FORMAT_XZ, memlimit=MEMORY_LIMIT)
    raw = decoder.decompress(packed, max_length=EXPANDED_LIMIT + 1)
    if (len(raw) > EXPANDED_LIMIT or not decoder.eof or decoder.unused_data
            or decoder.check != lzma.CHECK_CRC64):
        raise ValueError("invalid bounded XZ stream")
    return raw


def _unique_fixed(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate fixed field")
        result[key] = value
    return result


def _fixed_names(mode):
    if type(mode) is not str or mode not in SOURCE_BUNDLES:
        raise ValueError("fixed source mode")
    return set(SOURCE_BUNDLES[mode]["fixed"])


def encode_fixed(values, mode):
    if type(values) is not dict or set(values) != _fixed_names(mode):
        raise ValueError("fixed source inventory")
    files = {}
    for name, data in values.items():
        if type(data) is not bytes or len(data) > 65536:
            raise ValueError("fixed source size")
        files[name] = {"text": data.decode("utf-8", errors="strict"),
                       "sha256": hashlib.sha256(data).hexdigest()}
    raw = json.dumps({"files": files}, ensure_ascii=False, separators=(",", ":")).encode()
    return {"encoded": encode(raw), "expanded": len(raw)}


def decode_fixed(encoded, mode):
    raw = decode(encoded)
    frame = json.loads(raw.decode("utf-8", errors="strict"), object_pairs_hook=_unique_fixed)
    if (type(frame) is not dict or set(frame) != {"files"}
            or type(frame["files"]) is not dict or set(frame["files"]) != _fixed_names(mode)):
        raise ValueError("fixed source inventory")
    prepared = {}
    for name, value in frame["files"].items():
        if (type(value) is not dict or set(value) != {"text", "sha256"}
                or type(value["text"]) is not str or type(value["sha256"]) is not str):
            raise ValueError("fixed source shape")
        data = value["text"].encode("utf-8", errors="strict")
        if len(data) > 65536 or hashlib.sha256(data).hexdigest() != value["sha256"]:
            raise ValueError("fixed source binding")
        prepared[name] = data
    return raw, prepared


def bootstrap_command(source):
    """Quote bounded bootstrap data for one fixed owned path, without execution."""
    if type(source) is not str:
        raise ValueError("bootstrap heredoc source")
    data = source.encode("utf-8", errors="strict")
    delimiter = "KANZ_GATEWAY_FIXED_BOOTSTRAP_V1"
    if (len(data) > 65536 or "\x00" in source
            or any(line in (delimiter, delimiter + "\r") for line in source.split("\n"))):
        raise ValueError("bootstrap heredoc source")
    # The quoted body is data. Terminal X prevents command substitution from
    # removing source newlines; %X removes only that one appended literal byte.
    # A failing cat assignment or output redirection must stop the set -e caller.
    return ("bootstrap=$(/usr/bin/cat <<'" + delimiter + "'\n" + source + "X\n"
            + delimiter + "\n)\n"
            "printf '%s' \"${bootstrap%X}\" > \"$work/bootstrap.py\"\n")


def remote_decoder_source(source, mode):
    """Closed decoder projection from bounded, caller-supplied canonical source.

    The operator supplies its already qualified codec bytes as data. Only the
    four reviewed decoder functions and their closed dependencies travel; no
    encoder, arbitrary function selector, file lookup or source rewriting does.
    """
    if (type(source) is not str or len(source.encode("utf-8")) > 65536
            or type(mode) is not str or mode not in ("normal", "plan", "restore")):
        raise ValueError("remote decoder input")
    tree = ast.parse(source)
    functions = {}
    imports = {}
    assignments = {}
    gates = []
    all_functions = {"encode", "decode", "_unique_fixed", "_fixed_names",
                     "encode_fixed", "decode_fixed", "bootstrap_command", "remote_decoder_source"}
    all_imports = {"base64", "lzma", "hashlib", "json", "ast"}
    constants = {"ENCODED_LIMIT": 20000, "EXPANDED_LIMIT": 524288, "MEMORY_LIMIT": 16777216}
    for node in tree.body:
        if isinstance(node, ast.FunctionDef):
            if node.name not in all_functions or node.name in functions or node.decorator_list:
                raise ValueError("remote decoder function binding")
            functions[node.name] = node
        elif isinstance(node, ast.Import):
            if (len(node.names) != 1 or node.names[0].asname is not None
                    or node.names[0].name not in all_imports or node.names[0].name in imports):
                raise ValueError("remote decoder import binding")
            imports[node.names[0].name] = node
        elif isinstance(node, ast.Assign):
            if (len(node.targets) != 1 or not isinstance(node.targets[0], ast.Name)
                    or node.targets[0].id not in {*constants, "SOURCE_BUNDLES"}
                    or node.targets[0].id in assignments):
                raise ValueError("remote decoder constant binding")
            assignments[node.targets[0].id] = node
        elif isinstance(node, ast.If):
            gates.append(node)
        elif not (isinstance(node, ast.Expr) and isinstance(node.value, ast.Constant)
                  and isinstance(node.value.value, str) and node is tree.body[0]):
            raise ValueError("remote decoder module shape")
    if (set(functions) != all_functions or set(imports) != all_imports
            or set(assignments) != {*constants, "SOURCE_BUNDLES"} or len(gates) != 1):
        raise ValueError("remote decoder dependency inventory")
    for name, expected in constants.items():
        actual = ast.literal_eval(assignments[name].value)
        if type(actual) is not int or actual != expected:
            raise ValueError("remote decoder bound")
    gate = ast.parse('if not lzma.is_check_supported(lzma.CHECK_CRC64):\n'
                     '    raise ValueError("XZ CRC64 support required")').body[0]
    if ast.dump(gates[0], include_attributes=False) != ast.dump(gate, include_attributes=False):
        raise ValueError("remote decoder CRC64 gate")
    table_node = assignments["SOURCE_BUNDLES"].value
    for node in ast.walk(table_node):
        if isinstance(node, ast.Dict):
            keys = [ast.literal_eval(key) for key in node.keys]
            if any(type(key) is not str for key in keys) or len(keys) != len(set(keys)):
                raise ValueError("remote decoder partition binding")
    table = ast.literal_eval(table_node)
    if type(table) is not dict or set(table) != {"normal", "plan", "restore"} or table != SOURCE_BUNDLES:
        raise ValueError("remote decoder partition binding")
    for name, inventory in table.items():
        if (type(inventory) is not dict or set(inventory) != {"files", "fixed"}
                or type(inventory["files"]) is not list or type(inventory["fixed"]) is not list):
            raise ValueError("remote decoder partition shape")
        names = inventory["files"] + inventory["fixed"]
        if (len(inventory["files"]) != (7 if name == "normal" else 6)
                or len(inventory["fixed"]) != (1 if name == "plan" else 2)
                or any(type(value) is not str or not value.endswith(".py")
                       or not value[:-3] or any(c not in "abcdefghijklmnopqrstuvwxyz_" for c in value[:-3])
                       for value in names) or len(names) != len(set(names))):
            raise ValueError("remote decoder partition shape")
    dependencies = {
        "decode": {"ENCODED_LIMIT", "EXPANDED_LIMIT", "MEMORY_LIMIT", "ValueError", "base64", "isinstance", "len", "lzma", "str"},
        "_unique_fixed": {"ValueError"},
        "_fixed_names": {"SOURCE_BUNDLES", "ValueError", "set", "str", "type"},
        "decode_fixed": {"ValueError", "_fixed_names", "_unique_fixed", "decode", "dict", "hashlib", "json", "len", "set", "str", "type"},
    }
    signatures = {"decode": ("encoded",), "_unique_fixed": ("pairs",),
                  "_fixed_names": ("mode",), "decode_fixed": ("encoded", "mode")}
    for name, expected in dependencies.items():
        function = functions[name]
        if (function.args.posonlyargs or function.args.kwonlyargs
                or function.args.vararg is not None or function.args.kwarg is not None
                or tuple(argument.arg for argument in function.args.args) != signatures[name]
                or function.args.defaults or any(value is not None for value in function.args.kw_defaults)
                or function.returns is not None or getattr(function, "type_params", [])
                or any(argument.annotation is not None for argument in ast.walk(function.args)
                       if isinstance(argument, ast.arg))):
            raise ValueError("remote decoder signature")
        body = [node for statement in function.body for node in ast.walk(statement)]
        if any(isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef, ast.Lambda,
                                 ast.ListComp, ast.SetComp, ast.DictComp, ast.GeneratorExp,
                                 ast.Import, ast.ImportFrom, ast.Global, ast.Nonlocal)) for node in body):
            raise ValueError("remote decoder scoped dependency")
        local = {argument.arg for argument in ast.walk(function.args) if isinstance(argument, ast.arg)}
        local |= {node.id for node in body if isinstance(node, ast.Name) and isinstance(node.ctx, (ast.Store, ast.Del))}
        actual = {node.id for node in body if isinstance(node, ast.Name) and isinstance(node.ctx, ast.Load)} - local
        if actual != expected:
            raise ValueError("remote decoder global dependency")
    selected = {mode: table[mode]}
    nodes = [imports[name] for name in ("base64", "lzma", "hashlib", "json")]
    nodes += [gates[0], *(assignments[name] for name in constants)]
    parts = [ast.get_source_segment(source, node) for node in nodes]
    parts.append("SOURCE_BUNDLES = " + repr(selected))
    parts += [ast.get_source_segment(source, functions[name]) for name in dependencies]
    if any(type(part) is not str or not part for part in parts):
        raise ValueError("remote decoder source segment")
    return "\n\n".join(parts) + "\n"
