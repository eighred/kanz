#[path = "support/go_corpus.rs"]
mod go_corpus;
#[test]
fn go_okx_wire_updates_reconstruct_identical_books() {
    // Positive sequences share Go semantics; observed zero resets are exercised
    // separately because Go's legacy kernel treats zero as omitted.
    go_corpus::verify("KANZ_OKX_DEPTH_CORPUS", 2, 4, "OKX");
}
