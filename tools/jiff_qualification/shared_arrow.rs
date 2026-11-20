use sad_chrono::NaiveDateTime;

#[test]
fn both_arrow_versions_share_the_published_temporal_type() {
    for nanos in [i64::MIN, -1, 0, 1, i64::MAX] {
        let writer: NaiveDateTime =
            arrow_array::temporal_conversions::timestamp_ns_to_datetime(nanos).unwrap();
        let compute: NaiveDateTime =
            datafusion::arrow::temporal_conversions::timestamp_ns_to_datetime(nanos).unwrap();

        assert_eq!(writer, compute);
        assert_eq!(writer.and_utc().timestamp_nanos_opt(), Some(nanos));
        let instant = compute.and_utc();
        let reconstructed = i128::from(instant.timestamp()) * 1_000_000_000
            + i128::from(instant.timestamp_subsec_nanos());
        assert_eq!(reconstructed, i128::from(nanos));
    }
}
