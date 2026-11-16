"""Lossless, bounded XZ transport for the fixed operator source bundle."""
import base64
import lzma

if not lzma.is_check_supported(lzma.CHECK_CRC64):
    raise ValueError("XZ CRC64 support required")

ENCODED_LIMIT = 20000
EXPANDED_LIMIT = 524288
MEMORY_LIMIT = 16777216


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
