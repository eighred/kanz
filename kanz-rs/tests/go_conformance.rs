use kanz_decimal_compat::{common::v1::Decimal, compare, in_domain, is_positive, is_zero};
use prost::Message;
use serde::Deserialize;
use std::cmp::Ordering;
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Value {
    wire: Option<Vec<u8>>,
    domain: bool,
    positive: bool,
    zero: bool,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Corpus {
    version: u32,
    values: Vec<Value>,
    comparisons: Vec<i32>,
}
#[test]
#[ignore = "requires explicit Go oracle export and DECIMAL_CONFORMANCE_FIXTURE"]
fn matches_go_oracle() {
    let path =
        std::env::var("DECIMAL_CONFORMANCE_FIXTURE").expect("explicit Go fixture path is required");
    let bytes = std::fs::read(path).expect("read Go fixture");
    let corpus: Corpus = serde_json::from_slice(&bytes).expect("parse Go fixture");
    assert_eq!(corpus.version, 1);
    assert_eq!(corpus.values.len(), 141);
    assert_eq!(
        corpus.comparisons.len(),
        corpus.values.len() * corpus.values.len()
    );
    let values: Vec<_> = corpus
        .values
        .iter()
        .map(|v| {
            v.wire
                .as_ref()
                .map(|wire| Decimal::decode(wire.as_slice()).expect("canonical protobuf decode"))
        })
        .collect();
    for (i, (expected, value)) in corpus.values.iter().zip(&values).enumerate() {
        assert_eq!(in_domain(value.as_ref()), expected.domain, "domain {i}");
        assert_eq!(
            is_positive(value.as_ref()),
            expected.positive,
            "positive {i}"
        );
        assert_eq!(is_zero(value.as_ref()), expected.zero, "zero {i}");
        for (j, b) in values.iter().enumerate() {
            let actual = match compare(value.as_ref(), b.as_ref()) {
                Ordering::Less => -1,
                Ordering::Equal => 0,
                Ordering::Greater => 1,
            };
            assert_eq!(
                actual,
                corpus.comparisons[i * values.len() + j],
                "comparison {i},{j}"
            );
        }
    }
}
