//! Isolated Jiff 1.0.1 API qualification for object-store timeout configuration.
//! Legacy oracles are fixed values from the humantime API and the independently
//! reviewed object_store PR 908. No legacy crate or production graph is linked.
use std::time::Duration;

use jiff::fmt::friendly::{SpanParser, SpanPrinter};
use jiff::{SignedDuration, civil};

const PARSER: SpanParser = SpanParser::new();
const PRINTER: SpanPrinter = SpanPrinter::new();
// Qualification only. Jiff performs parsing and arithmetic. These checks cover
// the documented legacy fractional units and bare zero. They do not authorize
// replacing object_store's parser or its externally visible formatter.
fn qualify_elapsed(input: &str) -> Result<Duration, Box<dyn std::error::Error>> {
    if input == "0" {
        return Ok(Duration::ZERO);
    }
    let elapsed = PARSER.parse_unsigned_duration(input)?;
    if let Some((_, tail)) = input.rsplit_once('.') {
        let (digits, unit) = tail.split_at(tail.bytes().take_while(u8::is_ascii_digit).count());
        let precision = match unit.trim() {
            "ns" | "nsec" => 0,
            "us" | "µs" | "usec" => 3,
            "ms" | "msec" => 6,
            "s" | "sec" | "second" | "seconds" => 9,
            "m" | "min" | "minute" | "minutes" | "h" | "hr" | "hrs" | "hour" | "hours" => {
                if elapsed.subsec_nanos() != 0 {
                    return Err("fractional minutes and hours must resolve to whole seconds".into());
                }
                9
            }
            _ => return Err("unqualified fractional unit".into()),
        };
        if digits.trim_end_matches('0').len() > precision {
            return Err("elapsed value is smaller than nanosecond resolution".into());
        }
    }
    if input.contains(',') {
        return Err("comma syntax is outside the legacy overlap".into());
    }
    Ok(elapsed)
}

#[test]
fn qualified_elapsed_keeps_exact_common_config_values() {
    for (input, expected) in [
        ("0", Duration::ZERO),
        ("60s", Duration::from_secs(60)),
        ("1.5min", Duration::from_secs(90)),
        ("1.5hrs", Duration::from_secs(5_400)),
        ("500ms", Duration::from_millis(500)),
        ("0.001us", Duration::from_nanos(1)),
        ("1.0000001s", Duration::new(1, 100)),
    ] {
        assert_eq!(qualify_elapsed(input).unwrap(), expected, "{input}");
    }
    for input in ["0.0001us", "1.0001us", "0.0000001ms", "0.0001h", "0,0001us"] {
        assert!(qualify_elapsed(input).is_err(), "{input}");
    }
}

#[test]
fn exact_subseconds_and_whitespace() {
    for (input, expected) in [
        ("60s", Duration::from_secs(60)),
        ("60 seconds", Duration::from_secs(60)),
        ("1h30m", Duration::from_secs(5_400)),
        ("1 h 30 min", Duration::from_secs(5_400)),
        ("1s 2ms 3us 4ns", Duration::new(1, 2_003_004)),
        ("1s2ms3µs4ns", Duration::new(1, 2_003_004)),
        ("1.234567891s", Duration::new(1, 234_567_891)),
        ("0.000000001s", Duration::from_nanos(1)),
        ("0s", Duration::ZERO),
    ] {
        assert_eq!(PARSER.parse_unsigned_duration(input).unwrap(), expected, "{input}");
        let signed = PARSER.parse_duration(input).unwrap();
        assert_eq!(Duration::try_from(signed).unwrap(), expected, "{input}");
    }
}

#[test]
fn fixed_object_store_months_and_years_are_not_calendar_spans() {
    // humantime defines 30.44 and 365.25 fixed 24-hour days, respectively.
    let fixed_month = Duration::from_secs(2_630_016);
    let fixed_year = Duration::from_secs(31_557_600);
    let month = PARSER.parse_span("1 month").unwrap();
    let year = PARSER.parse_span("1 year").unwrap();
    for (span, reference, days, fixed) in [
        (month, civil::date(2024, 2, 1), 29, fixed_month),
        (month, civil::date(2024, 5, 1), 31, fixed_month),
        (year, civil::date(2024, 1, 1), 366, fixed_year),
        (year, civil::date(2025, 1, 1), 365, fixed_year),
    ] {
        let actual = Duration::try_from(span.to_duration(reference).unwrap()).unwrap();
        assert_eq!(actual, Duration::from_secs(days * 86_400));
        assert_ne!(actual, fixed);
    }
    for input in ["1 month", "1 year", "1d", "1w"] {
        assert!(PARSER.parse_unsigned_duration(input).is_err(), "{input}");
        assert!(PARSER.parse_duration(input).is_err(), "{input}");
    }
}

#[test]
fn old_accepted_grammar_cannot_pass_unmodified_to_jiff() {
    for (input, legacy) in [
        ("0", Duration::ZERO),
        ("1M", Duration::from_secs(2_630_016)),
        ("0.5d", Duration::from_secs(43_200)),
        ("1s 1h", Duration::from_secs(3_601)),
        ("1h 1h", Duration::from_secs(7_200)),
        ("1.5h 30m", Duration::from_secs(7_200)),
    ] {
        // A raw Jiff result must never silently replace the legacy value.
        assert_ne!(PARSER.parse_unsigned_duration(input).ok(), Some(legacy), "{input}");
    }
}

#[test]
fn text_roundtrips_and_externally_visible_formatting() {
    for (value, old_text, new_text) in [
        (Duration::from_micros(3), "3us", "3µs"),
        (Duration::from_secs(90_061), "1day 1h 1m 1s", "25h 1m 1s"),
        (Duration::new(1, 2_003_004), "1s 2ms 3us 4ns", "1s 2ms 3µs 4ns"),
    ] {
        assert_eq!(PRINTER.unsigned_duration_to_string(&value), new_text);
        assert_ne!(old_text, new_text);
        assert_eq!(PARSER.parse_unsigned_duration(new_text).unwrap(), value);
    }
}

#[test]
fn duration_range_signedness_and_explicit_rounding_probe() {
    assert!(Duration::try_from(PARSER.parse_duration("-1ns").unwrap()).is_err());
    assert!(PARSER.parse_unsigned_duration("-1ns").is_err());
    assert!(SignedDuration::try_from(Duration::MAX).is_err());
    assert!(PARSER.parse_unsigned_duration("18446744073709551616s").is_err());
    assert_eq!(PARSER.parse_duration("9223372036854775807s").unwrap(), SignedDuration::from_secs(i64::MAX));
    assert!(PARSER.parse_duration("9223372036854775808s").is_err());
    assert_eq!(PARSER.parse_unsigned_duration("9223372036854775808s").unwrap(), Duration::from_secs(1u64 << 63));
    assert_eq!(PARSER.parse_unsigned_duration("18446744073709551615s").unwrap(), Duration::from_secs(u64::MAX));
    assert!(PARSER.parse_unsigned_duration("0.1ns").is_err());
    assert_eq!(PARSER.parse_unsigned_duration("0.0001us").unwrap(), Duration::ZERO);
    assert!(PARSER.parse_unsigned_duration("0.0000000001s").is_err());
    assert!(PARSER.parse_unsigned_duration("0.0000000009s").is_err());
    let printed = PRINTER.unsigned_duration_to_string(&Duration::MAX);
    assert_eq!(printed, "5124095576030431h 15s 999ms 999µs 999ns");
    assert_eq!(PARSER.parse_unsigned_duration(&printed).unwrap(), Duration::MAX);
}
