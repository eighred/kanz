//! Fresh Go source/book corpus is mandatory; absence is a failure, not a skip.
use kanz_binance_depth::book::Book;
use kanz_binance_depth::proto::market::v1::{OrderBookDelta, OrderBookSnapshot};
use prost::Message;
use serde::Deserialize;

#[derive(Deserialize)]
struct Corpus {
    version: u32,
    cases: Vec<Case>,
}
#[derive(Deserialize)]
struct Case {
    name: String,
    steps: Vec<Step>,
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Step {
    kind: String,
    wire: Vec<u8>,
    book_wire: Vec<u8>,
}

#[test]
fn go_generated_wire_updates_reconstruct_identical_books() {
    let path = std::env::var("KANZ_DEPTH_CORPUS")
        .expect("set KANZ_DEPTH_CORPUS to fresh go run ./tools/depth-conformance output");
    let corpus: Corpus = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
    assert_eq!(corpus.version, 1);
    assert!(corpus.cases.len() >= 4, "missing compatibility cases");
    let mut count = 0;
    for case in corpus.cases {
        assert!(!case.steps.is_empty(), "empty case {}", case.name);
        let mut book = None;
        for step in case.steps {
            let expected = OrderBookSnapshot::decode(step.book_wire.as_slice()).unwrap();
            let book = book.get_or_insert_with(|| {
                Book::new(
                    expected.instrument_id.clone(),
                    expected.symbol.clone(),
                    expected.mic.clone(),
                )
            });
            match step.kind.as_str() {
                "snapshot" => book
                    .apply_snapshot(&OrderBookSnapshot::decode(step.wire.as_slice()).unwrap())
                    .unwrap(),
                "delta" => book
                    .apply_delta(&OrderBookDelta::decode(step.wire.as_slice()).unwrap())
                    .unwrap(),
                kind => panic!("unsupported corpus step {kind}"),
            }
            assert_eq!(
                book.snapshot(0),
                expected,
                "case {} step {count}",
                case.name
            );
            count += 1;
        }
    }
    assert!(count >= 11, "missing Go source/book steps");
}
