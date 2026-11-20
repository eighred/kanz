use datafusion::arrow::array::timezone::{tzdb_version, Tz};
use datafusion::arrow::datatypes::{DataType, Field};
use datafusion::common::{config::ConfigOptions, ScalarValue};
use datafusion::functions::datetime::to_timestamp_seconds;
use datafusion::logical_expr::{ColumnarValue, ScalarFunctionArgs};
use std::sync::Arc;

fn timestamp(input: &str) -> datafusion::error::Result<i64> {
    let config = Arc::new(ConfigOptions::default());
    let function = to_timestamp_seconds(&config);
    let fields = vec![Arc::new(Field::new("arg", DataType::Utf8, false)); 2];
    let result = function
        .inner()
        .return_type(&[DataType::Utf8, DataType::Utf8])?;
    let args = [input, "%Y-%m-%d %H:%M:%S %Z"]
        .into_iter()
        .map(|value| ColumnarValue::Scalar(ScalarValue::Utf8(Some(value.into()))))
        .collect();
    let value = function.invoke_with_args(ScalarFunctionArgs {
        args,
        arg_fields: fields,
        number_rows: 1,
        return_field: Arc::new(Field::new("result", result, true)),
        config_options: config,
    })?;
    match value {
        ColumnarValue::Scalar(ScalarValue::TimestampSecond(Some(seconds), _)) => Ok(seconds),
        other => panic!("unexpected timestamp result: {other:?}"),
    }
}

#[test]
fn named_zone_timestamps() {
    fn require_copy<T: Copy>(_: T) {}
    require_copy("America/Los_Angeles".parse::<Tz>().unwrap());
    assert_eq!(tzdb_version(), Some("2026f"));
    for (input, expected) in [
        ("2025-07-01 12:00:00 America/Los_Angeles", 1751396400),
        ("2200-01-01 12:00:00 America/Los_Angeles", 7258190400),
        ("2200-07-01 12:00:00 America/Los_Angeles", 7273825200),
        ("2200-01-01 12:00:00 Australia/Sydney", 7258122000),
        ("2200-07-01 12:00:00 Australia/Sydney", 7273764000),
        // Released DataFusion falls back to the configured timezone for these local times.
        ("2025-03-09 02:30:00 America/Los_Angeles", 1741487400),
        ("2025-11-02 01:30:00 America/Los_Angeles", 1762047000),
    ] {
        assert_eq!(timestamp(input).unwrap(), expected, "{input}");
    }
    for (alias, canonical) in [
        ("US/Pacific", "America/Los_Angeles"),
        ("Japan", "Asia/Tokyo"),
        ("UTC", "Etc/UTC"),
    ] {
        let parse = |zone| timestamp(&format!("2025-05-01 12:00:00 {zone}")).unwrap();
        assert_eq!(parse(alias), parse(canonical));
    }
    for invalid in [
        "Unknown/Zone",
        "america/los_angeles",
        "Factory",
        "+09:00",
        "-04",
        "+0230",
    ] {
        let error = timestamp(&format!("2025-05-01 12:00:00 {invalid}")).unwrap_err();
        assert!(
            error.to_string().ends_with("failed to parse timezone"),
            "{invalid}"
        );
    }
}
