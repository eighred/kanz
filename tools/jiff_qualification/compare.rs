use std::fs::File;
use std::net::UdpSocket;
use std::os::raw::c_int;

use jiff::Timestamp;
use jiff::tz::{AmbiguousOffset, TimeZone};

unsafe extern "C" {
    fn close_io() -> c_int;
}

struct Dataset {
    names: &'static str,
    tzif: &'static [u8],
    utc: &'static str,
    civil: &'static str,
}

macro_rules! dataset {
    ($version:literal) => {
        Dataset {
            names: include_str!(concat!($version, "/zones.tsv")),
            tzif: include_bytes!(concat!($version, "/corpus.bin")),
            utc: include_str!(concat!($version, "/utc.tsv")),
            civil: include_str!(concat!($version, "/civil.tsv")),
        }
    };
}

fn parse_zones(data: &Dataset, bundled: bool) -> Vec<(&'static str, TimeZone)> {
    let mut zones = Vec::new();
    for line in data.names.lines() {
        let [name, start, end]: [&str; 3] = fields(line);
        let start: usize = start.parse().unwrap();
        let end: usize = end.parse().unwrap();
        let tzif = &data.tzif[start..end];
        let zone = if bundled {
            let (_, distributed_bytes) = jiff_tzdb::get(name).unwrap();
            assert_eq!(tzif, distributed_bytes, "independent corpus diverged for {name}");
            TimeZone::get(name).unwrap()
        } else {
            TimeZone::tzif(name, tzif).unwrap()
        };
        zones.push((name, zone));
    }
    zones
}

fn fields<const N: usize>(line: &str) -> [&str; N] {
    line.split('\t').collect::<Vec<_>>().try_into().unwrap()
}

fn utc_candidates(zone: &TimeZone, civil_seconds: i64) -> Vec<i64> {
    // The oracle encodes a local civil date as seconds on a UTC calendar.
    let local = TimeZone::UTC.to_datetime(Timestamp::from_second(civil_seconds).unwrap());
    let mapping = zone.to_ambiguous_timestamp(local);
    match mapping.offset() {
        AmbiguousOffset::Unambiguous { .. } => vec![mapping.unambiguous().unwrap().as_second()],
        AmbiguousOffset::Gap { .. } => Vec::new(),
        AmbiguousOffset::Fold { .. } => vec![
            mapping.clone().earlier().unwrap().as_second(),
            mapping.later().unwrap().as_second(),
        ],
    }
}

fn compare(data: Dataset, bundled: bool, expected_counts: (usize, usize, usize)) {
    let zones = parse_zones(&data, bundled);
    let mut utc_count = 0;
    for line in data.utc.lines() {
        let [index, seconds, offset] = fields(line);
        let (name, zone) = &zones[index.parse::<usize>().unwrap()];
        let instant = Timestamp::from_second(seconds.parse().unwrap()).unwrap();
        let expected = offset.parse::<i32>().unwrap();
        assert_eq!(zone.to_offset(instant).seconds(), expected, "{name} at {instant}");
        utc_count += 1;
    }

    let mut civil_count = 0;
    for line in data.civil.lines() {
        let [index, seconds, candidates] = fields(line);
        let (name, zone) = &zones[index.parse::<usize>().unwrap()];
        let expected: Vec<i64> = candidates
            .split(',')
            .filter(|value| !value.is_empty())
            .map(|value| value.parse().unwrap())
            .collect();
        assert_eq!(utc_candidates(zone, seconds.parse().unwrap()), expected, "{name} civil {seconds}");
        civil_count += 1;
    }
    assert_eq!((zones.len(), utc_count, civil_count), expected_counts);

    let casablanca = zones.iter().find(|(name, _)| *name == "Africa/Casablanca").unwrap();
    let sample: Timestamp = "2039-01-14T12:34:56Z".parse().unwrap();
    assert_eq!(casablanca.1.to_offset(sample).seconds(), if bundled { 0 } else { 3600 });
    println!("bundled={bundled} names={} UTC={utc_count} civil={civil_count}", zones.len());
}

fn main() {
    // Prove that an otherwise readable path cannot be reopened after confinement.
    File::open("/src/closed.c").unwrap();
    assert_eq!(unsafe { close_io() }, 0);
    assert_eq!(File::open("/src/closed.c").unwrap_err().raw_os_error(), Some(1));
    assert_eq!(UdpSocket::bind("127.0.0.1:0").unwrap_err().raw_os_error(), Some(1));
    assert_eq!(jiff_tzdb::VERSION, "2026f");
    assert!(jiff_tzdb::get("missing/example").is_none());

    // The worker's Protobuf seconds/nanos representation exceeds Jiff's upper bound.
    assert!(Timestamp::from_second(-62_135_596_800).is_ok());
    assert!(Timestamp::new(253_402_300_799, 999_999_999).is_err());
    assert_eq!(Timestamp::MAX.to_string(), "9999-12-30T22:00:00.999999999Z");
    compare(dataset!("2025c"), false, (598, 112_238, 84_497));
    compare(dataset!("2026f"), true, (598, 112_505, 84_710));
}
