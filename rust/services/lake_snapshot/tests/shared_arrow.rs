fn same_type<T>(first: T, second: T) -> (T, T) {
    (first, second)
}

#[test]
fn both_arrow_versions_share_the_published_temporal_type() {
    for nanos in [i64::MIN, -1, 0, 1, i64::MAX] {
        let writer = arrow_array::temporal_conversions::timestamp_ns_to_datetime(nanos).unwrap();
        let compute =
            datafusion::arrow::temporal_conversions::timestamp_ns_to_datetime(nanos).unwrap();

        let (writer, compute) = same_type(writer, compute);
        assert_eq!(writer, compute);
        assert_eq!(writer.and_utc().timestamp_nanos_opt(), Some(nanos));
        let instant = compute.and_utc();
        let reconstructed = i128::from(instant.timestamp()) * 1_000_000_000
            + i128::from(instant.timestamp_subsec_nanos());
        assert_eq!(reconstructed, i128::from(nanos));
    }
}
