"""Generate independent UTC-offset and civil-time vectors using CPython zoneinfo."""
import datetime as dt
import io
import pathlib
import re
import struct
import sys
import zipfile
from zoneinfo import ZoneInfo

ROOT = pathlib.Path(__file__).parent
EPOCH = dt.datetime(1970, 1, 1)
WEEK = 7 * 86400
CIVIL_CASES = (
    ("America/New_York", "2024-03-10 02:30"),
    ("America/New_York", "2024-11-03 01:30"),
    ("Australia/Lord_Howe", "2024-04-07 01:45"),
    ("Australia/Lord_Howe", "2024-10-06 02:15"),
    ("Asia/Kathmandu", "2024-04-07 01:00"),
    ("Pacific/Apia", "2011-12-30 13:00"),
    ("America/New_York", "2100-03-14 02:30"),
    ("America/New_York", "2100-11-07 01:30"),
)


def read_zones(version):
    if version == "2025c":
        with zipfile.ZipFile(ROOT / "zoneinfo.zip") as archive:
            return {key: archive.read(key) for key in archive.namelist()}
    if version != "2026f":
        raise ValueError(f"unexpected corpus {version}")
    source = ROOT / "src" / "jiff-tzdb"
    table = (source / "tzname.rs").read_text()
    data = (source / "concatenated-zoneinfo.dat").read_bytes()
    if 'static VERSION: &str = "2026f";' not in table:
        raise ValueError("unexpected bundled IANA version")
    rows = re.findall(r'^\s+\(r"([^"]+)", (\d+)\.\.(\d+)\),$', table, re.M)
    zones = {key: data[int(start):int(end)] for key, start, end in rows}
    if len(rows) != len(zones) or len(rows) != 598:
        raise ValueError("invalid bundled name inventory")
    return zones


def seconds(value):
    return (value - EPOCH) // dt.timedelta(seconds=1)


def utc_offset(zone, instant):
    utc = (EPOCH + dt.timedelta(seconds=instant)).replace(tzinfo=dt.UTC)
    return int(utc.astimezone(zone).utcoffset().total_seconds())


def utc_candidates(zone, civil):
    candidates = set()
    for fold in (0, 1):
        utc = civil.replace(tzinfo=zone, fold=fold).astimezone(dt.UTC)
        if utc.astimezone(zone).replace(tzinfo=None) == civil:
            candidates.add(seconds(utc.replace(tzinfo=None)))
    return ",".join(str(value) for value in sorted(candidates))


def transitions(data, zone):
    # RFC 9636 places the 64-bit TZif transition array after the first header/block.
    # We read the instants only. CPython computes every expected offset.
    ut, standard, leap, count, types, names = struct.unpack_from(">6I", data, 20)
    second_header = 44 + ut + standard + leap * 8 + count * 5 + types * 6 + names
    count = struct.unpack_from(">I", data, second_header + 32)[0]
    yield from struct.unpack_from(f">{count}q", data, second_header + 44)

    # The pinned corpora have more than seven days between changes in 2100.
    # Bracket each future transition without interpreting the POSIX TZif footer.
    january = seconds(dt.datetime(2100, 1, 1))
    previous = january
    previous_offset = utc_offset(zone, previous)
    for now in range(january + WEEK, january + 372 * 86400, WEEK):
        current_offset = utc_offset(zone, now)
        if current_offset != previous_offset:
            low, high = previous, now
            while high - low > 1:
                middle = (low + high) // 2
                if utc_offset(zone, middle) == previous_offset:
                    low = middle
                else:
                    high = middle
            yield high
        previous, previous_offset = now, current_offset


def generate(version):
    out = ROOT / version
    out.mkdir()
    corpus = bytearray()
    zones, utc, civil = [], [], []
    named = {}
    for index, (name, data) in enumerate(sorted(read_zones(version).items())):
        zone = ZoneInfo.from_file(io.BytesIO(data), key=name)
        named[name] = (index, zone)
        zones.append(f"{name}\t{len(corpus)}\t{len(corpus) + len(data)}\n")
        corpus.extend(data)
        for year in (2, 1850, 1900, 1943, 1970, 2000, 2024, 2039, 2100, 2500, 9998):
            for month in (1, 4, 7, 10):
                instant = seconds(dt.datetime(year, month, 14, 12, 34, 56))
                utc.append(f"{index}\t{instant}\t{utc_offset(zone, instant)}\n")

        for instant in transitions(data, zone):
            # Leave sufficient room to convert the local dates on both sides.
            if not -62135500000 < instant < 253402000000:
                continue
            before = utc_offset(zone, instant - 1)
            after = utc_offset(zone, instant)
            for point in (instant - 1, instant, instant + 1):
                utc.append(f"{index}\t{point}\t{utc_offset(zone, point)}\n")
            if before != after:
                walls = (instant + min(before, after) - 1, instant + (before + after) // 2,
                         instant + max(before, after))
                for wall in sorted(set(walls)):
                    local = EPOCH + dt.timedelta(seconds=wall)
                    civil.append(f"{index}\t{wall}\t{utc_candidates(zone, local)}\n")

    for name, text in CIVIL_CASES:
        index, zone = named[name]
        local = dt.datetime.fromisoformat(text)
        civil.append(f"{index}\t{seconds(local)}\t{utc_candidates(zone, local)}\n")

    (out / "corpus.bin").write_bytes(corpus)
    for name, lines in (("zones", zones), ("utc", utc), ("civil", civil)):
        (out / f"{name}.tsv").write_text("".join(lines), encoding="ascii", newline="\n")
    print(f"{version}: {len(zones)} names, {len(utc)} UTC vectors, {len(civil)} civil vectors", flush=True)


if __name__ == "__main__":
    generate(sys.argv[1])
