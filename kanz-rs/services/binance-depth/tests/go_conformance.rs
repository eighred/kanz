#[path = "../../../crates/market-feed/tests/support/go_corpus.rs"]
mod go_corpus;
#[test]
fn go_generated_wire_updates_reconstruct_identical_books() {
    go_corpus::verify("KANZ_DEPTH_CORPUS", 4, 11, "BINANCE");
}
