//! Fresh test-only Go capture. Missing evidence is an error in the explicit CI gate.
use kanz_ledger_replay::{replay, Book, Entry, Error, TimePoint, MAX_ENTRIES};
use num_bigint::BigInt;
use num_rational::BigRational as Rat;
use serde::Deserialize;
use std::{collections::BTreeMap, io::Read};

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
struct Instant {
    seconds: i64,
    nanos: u32,
}
impl Instant {
    fn time(&self) -> TimePoint {
        TimePoint::new(self.seconds, self.nanos).unwrap()
    }
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Capture {
    id: String,
    portfolio: String,
    kind: i32,
    instrument: String,
    quantity: Option<String>,
    price: Option<String>,
    cash: Option<String>,
    currency: String,
    effective: Instant,
    knowledge: Instant,
    settlement_basis: i32,
    settlement_date: Instant,
    has_action: bool,
}
fn number(value: &str) -> Rat {
    assert!(value.len() <= 4096);
    value.parse().unwrap()
}
impl Capture {
    fn entry(self) -> Entry {
        Entry {
            id: self.id,
            portfolio: self.portfolio,
            kind: self.kind,
            instrument: self.instrument,
            quantity: self.quantity.as_deref().map(number),
            price: self.price.as_deref().map(number),
            cash: self.cash.as_deref().map(number),
            currency: self.currency,
            effective: self.effective.time(),
            knowledge: self.knowledge.time(),
            settlement_basis: self.settlement_basis,
            settlement_date: self.settlement_date.time(),
            has_action: self.has_action,
        }
    }
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ExpectedLot {
    quantity: String,
    average: String,
    realized: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Expected {
    positions: BTreeMap<String, ExpectedLot>,
    cash: BTreeMap<String, String>,
    accrued: BTreeMap<String, String>,
    settled_positions: BTreeMap<String, ExpectedLot>,
    settled_cash: BTreeMap<String, String>,
    unknown: usize,
    pending: usize,
    complete: bool,
}
impl Expected {
    fn book(self) -> Book {
        let lots = |m: BTreeMap<String, ExpectedLot>| {
            m.into_iter()
                .map(|(k, v)| {
                    (
                        k,
                        kanz_order_core::cost_basis::Lot {
                            qty: number(&v.quantity),
                            avg: number(&v.average),
                            realized: number(&v.realized),
                        },
                    )
                })
                .collect()
        };
        let amounts =
            |m: BTreeMap<String, String>| m.into_iter().map(|(k, v)| (k, number(&v))).collect();
        let b = Book {
            positions: lots(self.positions),
            cash: amounts(self.cash),
            accrued: amounts(self.accrued),
            settled_positions: lots(self.settled_positions),
            settled_cash: amounts(self.settled_cash),
            unknown_settlement: self.unknown,
            pending_settlement: self.pending,
        };
        assert_eq!(b.settlement_basis_complete(), self.complete);
        b
    }
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Query {
    name: String,
    effective: Instant,
    knowledge: Instant,
    expected: Expected,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct History {
    name: String,
    portfolio: String,
    entries: Vec<Option<Capture>>,
    queries: Vec<Query>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Corpus {
    version: u32,
    histories: Vec<History>,
}

#[test]
#[ignore = "requires freshly generated KANZ_LEDGER_CORPUS; CI invokes this gate explicitly"]
fn fresh_go_owner_matches_exactly_at_scale() {
    let path = std::env::var("KANZ_LEDGER_CORPUS").expect("fresh Go corpus is required");
    let mut raw = Vec::new();
    std::fs::File::open(path)
        .unwrap()
        .take(32 * 1024 * 1024 + 1)
        .read_to_end(&mut raw)
        .unwrap();
    assert!(raw.len() <= 32 * 1024 * 1024);
    let corpus: Corpus = serde_json::from_slice(&raw).unwrap();
    assert_eq!(corpus.version, 1);
    assert_eq!(corpus.histories.len(), 18);
    let mut entries_count = 0;
    let mut queries_count = 0;
    for h in corpus.histories {
        let entries: Vec<_> = h
            .entries
            .into_iter()
            .map(|e| e.map(Capture::entry))
            .collect();
        entries_count += entries.len();
        for q in h.queries {
            let actual = replay(
                &h.portfolio,
                &entries,
                Some(q.effective.time()),
                Some(q.knowledge.time()),
            )
            .unwrap();
            assert_eq!(actual, q.expected.book(), "{}/{}", h.name, q.name);
            queries_count += 1;
        }
        // Negative control: a changed economic leg must disagree with the Go oracle.
        if h.name == "hand-derived-mixed" {
            let expected = replay(&h.portfolio, &entries, None, None).unwrap();
            let mut changed = entries.clone();
            changed[1].as_mut().unwrap().cash = Some(number("999999"));
            assert_ne!(
                replay(&h.portfolio, &changed, None, None).unwrap(),
                expected
            );
        }
    }
    assert!(entries_count >= 17_000);
    assert_eq!(queries_count, 215);
    println!("Go/Rust exact parity: {entries_count} captured entries, {queries_count} bitemporal books, 18 histories");
}

fn entry(id: &str, q: &str, p: &str, c: &str) -> Option<Entry> {
    Some(Entry {
        id: id.into(),
        portfolio: "p".into(),
        kind: 1,
        instrument: "asset".into(),
        quantity: Some(number(q)),
        price: Some(number(p)),
        cash: Some(number(c)),
        currency: "USD".into(),
        effective: TimePoint::new(1, 0).unwrap(),
        knowledge: TimePoint::new(1, 0).unwrap(),
        settlement_basis: 1,
        settlement_date: TimePoint::ZERO,
        has_action: false,
    })
}
#[test]
fn independent_closed_trade_conservation_and_reversal() {
    // Buy 2*10 + 1*13: average 11. Sell 3*15: gross profit and cash = 12.
    let entries = vec![
        entry("a", "2", "10", "-20"),
        entry("b", "1", "13", "-13"),
        entry("c", "-3", "15", "45"),
    ];
    let b = replay("p", &entries, None, None).unwrap();
    let lot = &b.positions["asset"];
    assert_eq!(
        (&lot.qty, &lot.avg, &lot.realized),
        (&number("0"), &number("0"), &number("12"))
    );
    assert_eq!(b.cash["USD"], number("12"));
    assert_eq!(b.positions, b.settled_positions);
    let corrections = vec![
        entry("a", "2/7", "11/3", "-22/21"),
        entry("b", "-2/7", "11/3", "22/21"),
    ];
    let b = replay("p", &corrections, None, None).unwrap();
    assert_eq!(b.cash["USD"], number("0"));
    assert_eq!(b.positions["asset"], Default::default());
}
#[test]
fn knowledge_settlement_accrual_and_duplicate_boundaries() {
    let mut pending = entry("p", "1", "10", "-10").unwrap();
    pending.settlement_basis = 2;
    pending.settlement_date = TimePoint::new(0, 0).unwrap();
    let mut late = entry("late", "-1", "20", "20").unwrap();
    late.knowledge = TimePoint::new(2, 1).unwrap();
    let mut unknown = entry("unknown", "0", "0", "0").unwrap();
    unknown.settlement_basis = 99;
    let mut accrual = entry("income", "0", "0", "7/3").unwrap();
    accrual.kind = 5;
    let events = vec![
        Some(pending.clone()),
        Some(pending),
        Some(late),
        Some(unknown),
        Some(accrual),
    ];
    let b = replay("p", &events, None, Some(TimePoint::new(2, 0).unwrap())).unwrap();
    assert_eq!(b.positions["asset"].qty, number("1"));
    assert_eq!((b.unknown_settlement, b.pending_settlement), (1, 1));
    assert!(!b.settlement_basis_complete());
    assert!(b.settled_cash.is_empty());
    assert_eq!(b.accrued["USD"], number("7/3"));
    assert_eq!(b.cash["USD"], number("-10"));
}
#[test]
fn short_cross_and_stable_equal_key_order() {
    let mut a = entry("same", "-2", "10", "20").unwrap();
    let mut b = a.clone();
    b.quantity = Some(number("100"));
    a.portfolio.clear(); // legacy empty portfolio follows the caller's authorized scope
    let events = vec![Some(a), Some(b), entry("z", "3", "7", "-21")];
    let b = replay("p", &events, None, None).unwrap();
    assert_eq!(
        (
            b.positions["asset"].qty.clone(),
            b.positions["asset"].avg.clone(),
            b.positions["asset"].realized.clone()
        ),
        (number("1"), number("7"), number("6"))
    );
}
#[test]
fn unsupported_actions_refuse_before_effective_cutoff() {
    let mut action = entry("action", "1", "10", "-10").unwrap();
    action.has_action = true;
    action.effective = TimePoint::new(20, 0).unwrap();
    assert_eq!(
        replay(
            "p",
            &[Some(action.clone())],
            Some(TimePoint::new(10, 0).unwrap()),
            None
        ),
        Err(Error::Unsupported)
    );
    action.portfolio = "other".into();
    assert_eq!(
        replay("p", &[Some(action.clone())], None, None),
        Ok(Book::default())
    );
    action.portfolio = "p".into();
    action.knowledge = TimePoint::new(20, 0).unwrap();
    assert_eq!(
        replay(
            "p",
            &[Some(action)],
            None,
            Some(TimePoint::new(10, 0).unwrap())
        ),
        Ok(Book::default())
    );
    let mut action = entry("type", "0", "0", "0").unwrap();
    action.kind = 4;
    assert_eq!(
        replay("p", &[Some(action)], None, None),
        Err(Error::Unsupported)
    );
}
#[test]
fn limits_and_failed_cost_fold_are_atomic() {
    assert_eq!(
        replay("p", &vec![None; MAX_ENTRIES + 1], None, None),
        Err(Error::Capacity)
    );
    let mut e = entry("a", "1", "10", "-10").unwrap();
    e.id = "a".repeat(257);
    assert_eq!(replay("p", &[Some(e)], None, None), Err(Error::Capacity));
    let huge = Rat::from_integer(BigInt::from(1) << 16384);
    let mut lot = kanz_order_core::cost_basis::Lot::default();
    let before = lot.clone();
    assert!(lot.fold(&huge, &number("1")).is_err());
    assert_eq!(lot, before);
    // Both inputs fit, but their sum exceeds the output budget after arithmetic.
    lot.qty = Rat::from_integer(BigInt::from(1) << 16383);
    let before = lot.clone();
    assert!(lot.fold(&before.qty, &number("1")).is_err());
    assert_eq!(lot, before);
    assert!(TimePoint::new(0, 1_000_000_000).is_err());
    assert!(TimePoint::new(253_402_300_800, 0).is_err());
    assert!(TimePoint::new(-62_135_596_801, 0).is_err());
    assert!(TimePoint::new(253_402_300_799, 999_999_999).is_ok());
}
