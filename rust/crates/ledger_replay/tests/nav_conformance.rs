use kanz_ledger_replay::{
    valuation::{self, Error, Quotes, Valuation},
    Book,
};
use num_bigint::BigInt;
use num_rational::BigRational as Rat;
use serde::Deserialize;
use std::{collections::BTreeMap, io::Read};

fn n(s: &str) -> Rat {
    assert!(s.len() <= 4096);
    s.parse().unwrap()
}
fn quotes(m: BTreeMap<String, Option<String>>) -> Quotes {
    m.into_iter()
        .map(|(k, v)| (k, v.as_deref().map(n)))
        .collect()
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Expected {
    total: String,
    cash: String,
    securities: String,
    accrued: String,
    local: BTreeMap<String, String>,
}
impl Expected {
    fn value(&self) -> Valuation {
        Valuation {
            total: n(&self.total),
            cash: n(&self.cash),
            security_value: n(&self.securities),
            accrued: n(&self.accrued),
            local_exposure: self.local.iter().map(|(k, v)| (k.clone(), n(v))).collect(),
        }
    }
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Refusal {
    kind: String,
    names: Vec<String>,
}
impl Refusal {
    fn error(&self) -> Error {
        match self.kind.as_str() {
            "converter" => {
                assert!(self.names.is_empty());
                Error::MissingConverter
            }
            "price" => {
                assert_eq!(self.names.len(), 1);
                Error::MissingPrice(self.names[0].clone())
            }
            "fx" => Error::MissingFx(self.names.clone()),
            _ => panic!("unknown Go refusal"),
        }
    }
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Case {
    name: String,
    reporting: String,
    positions: BTreeMap<String, String>,
    cash: BTreeMap<String, String>,
    accrued: BTreeMap<String, String>,
    prices: BTreeMap<String, Option<String>>,
    currencies: BTreeMap<String, String>,
    rates: BTreeMap<String, Option<String>>,
    converter: bool,
    expected: Option<Expected>,
    refusal: Option<Refusal>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Corpus {
    version: u32,
    cases: Vec<Case>,
}

#[test]
#[ignore = "requires freshly generated KANZ_NAV_CORPUS; CI runs the explicit gate"]
fn fresh_go_nav_matches_values_and_named_refusals() {
    let path = std::env::var("KANZ_NAV_CORPUS").expect("fresh Go NAV corpus required");
    let mut raw = Vec::new();
    std::fs::File::open(path)
        .unwrap()
        .take(64 * 1024 * 1024 + 1)
        .read_to_end(&mut raw)
        .unwrap();
    assert!(raw.len() <= 64 * 1024 * 1024);
    let corpus: Corpus = serde_json::from_slice(&raw).unwrap();
    assert_eq!(corpus.version, 1);
    assert_eq!(corpus.cases.len(), 2061);
    let (mut holdings, mut passed, mut refused) = (0, 0, 0);
    for c in corpus.cases {
        let mut book = Book::default();
        holdings += c.positions.len();
        for (k, v) in c.positions {
            book.positions.entry(k).or_default().qty = n(&v)
        }
        book.cash = c.cash.into_iter().map(|(k, v)| (k, n(&v))).collect();
        book.accrued = c.accrued.into_iter().map(|(k, v)| (k, n(&v))).collect();
        let prices = quotes(c.prices);
        let mut rates = quotes(c.rates);
        let got = valuation::value(
            &book,
            &c.reporting,
            &prices,
            &c.currencies,
            c.converter.then_some(&rates),
        );
        match (c.expected, c.refusal) {
            (Some(want), None) => {
                let expected = want.value();
                assert_eq!(got, Ok(expected.clone()), "{}", c.name);
                passed += 1;
                assert_eq!(
                    expected.total,
                    &expected.cash + &expected.security_value + &expected.accrued
                );
                // Unit conversion control: changing the foreign FX quote must be observable.
                if c.name == "foreign-golden" {
                    rates.insert("EUR".into(), Some(n("6/5")));
                    assert_ne!(
                        valuation::value(&book, &c.reporting, &prices, &c.currencies, Some(&rates)),
                        Ok(expected)
                    );
                }
            }
            (None, Some(want)) => {
                assert_eq!(got, Err(want.error()), "{}", c.name);
                refused += 1;
            }
            _ => panic!("{} has no unique expected outcome", c.name),
        }
    }
    assert_eq!(holdings, 139269);
    assert!(passed > 1000 && refused > 500);
    println!("Go/Rust NAV parity: 2061 books, {holdings} holding inputs, {passed} exact valuations, {refused} named refusals");
}

fn cash_book(currency: &str, amount: &str) -> Book {
    let mut b = Book::default();
    b.cash.insert(currency.into(), n(amount));
    b
}
#[test]
fn independent_foreign_components_and_signed_fractional_units() {
    // 100 shares * EUR85 + EUR2000 cash + EUR5 accrued = EUR10505.
    // At 11/10 USD/EUR: securities 9350, cash 2200, accrual 11/2 USD.
    let mut b = cash_book("EUR", "2000");
    b.positions.entry("equity".into()).or_default().qty = n("100");
    b.accrued.insert("EUR".into(), n("5"));
    let prices = BTreeMap::from([("equity".into(), Some(n("85")))]);
    let currencies = BTreeMap::from([("equity".into(), "EUR".into())]);
    let rates = BTreeMap::from([("EUR".into(), Some(n("11/10")))]);
    let v = valuation::value(&b, "USD", &prices, &currencies, Some(&rates)).unwrap();
    assert_eq!(
        (v.total, v.cash, v.security_value, v.accrued),
        (n("23111/2"), n("2200"), n("9350"), n("11/2"))
    );
    assert_eq!(v.local_exposure["EUR"], n("10505"));
    b.positions.get_mut("equity").unwrap().qty = n("-1/7");
    b.cash.insert("EUR".into(), n("2"));
    b.accrued.clear();
    b.accrued.insert("USD".into(), n("-1"));
    let prices = BTreeMap::from([("equity".into(), Some(n("1/3")))]);
    let rates = BTreeMap::from([("EUR".into(), Some(n("7/11")))]);
    let v = valuation::value(&b, "USD", &prices, &currencies, Some(&rates)).unwrap();
    assert_eq!(v.security_value, n("-1/33"));
    assert_eq!(v.total, n("8/33"));
}
#[test]
fn missing_fx_cannot_hide_in_offsets_or_zero_marks() {
    let empty = BTreeMap::new();
    let mut b = cash_book("EUR", "100");
    b.accrued.insert("EUR".into(), n("-100"));
    b.cash.insert("GBP".into(), n("1"));
    b.accrued.insert("JPY".into(), n("1"));
    assert_eq!(
        valuation::value(&b, "USD", &empty, &BTreeMap::new(), Some(&empty)),
        Err(Error::MissingFx(vec![
            "EUR".into(),
            "GBP".into(),
            "JPY".into()
        ]))
    );
    let mut b = Book::default();
    b.positions.entry("equity".into()).or_default().qty = n("1");
    let prices = BTreeMap::from([("equity".into(), Some(n("0")))]);
    let currencies = BTreeMap::from([("equity".into(), "EUR".into())]);
    assert_eq!(
        valuation::value(&b, "USD", &prices, &currencies, Some(&empty)),
        Err(Error::MissingFx(vec!["EUR".into()]))
    );
}
#[test]
fn zero_holdings_identity_and_traded_basis_match_the_owner() {
    let mut b = cash_book("USD", "3");
    b.cash.insert("".into(), n("2"));
    b.accrued.insert("JPY".into(), n("0"));
    b.positions.entry("unpriced".into()).or_default();
    b.settled_cash.insert("EUR".into(), n("999999"));
    b.unknown_settlement = 10;
    let empty = BTreeMap::new();
    let rates = BTreeMap::from([("USD".into(), Some(n("-1"))), ("".into(), None)]);
    let v = valuation::value(&b, "USD", &empty, &BTreeMap::new(), Some(&rates)).unwrap();
    assert_eq!(v.total, n("5"));
    assert_eq!(v.local_exposure.len(), 2);
    assert_eq!(
        valuation::value(&b, "USD", &empty, &BTreeMap::new(), None),
        Err(Error::MissingConverter)
    );
    b.positions.get_mut("unpriced").unwrap().qty = n("1");
    assert_eq!(
        valuation::value(&b, "USD", &empty, &BTreeMap::new(), Some(&empty)),
        Err(Error::MissingPrice("unpriced".into()))
    );
}
#[test]
fn unusable_foreign_rates_refuse_and_no_partial_result_escapes() {
    let b = cash_book("EUR", "100");
    let empty = BTreeMap::new();
    for rate in [None, Some(n("0")), Some(n("-1/100"))] {
        let rates = BTreeMap::from([("EUR".into(), rate)]);
        assert_eq!(
            valuation::value(&b, "USD", &empty, &BTreeMap::new(), Some(&rates)),
            Err(Error::MissingFx(vec!["EUR".into()]))
        );
    }
    let mut b = cash_book("USD", "1");
    b.cash
        .insert("EUR".into(), Rat::new_raw(1.into(), 0.into()));
    assert_eq!(
        valuation::value(&b, "USD", &empty, &BTreeMap::new(), Some(&empty)),
        Err(Error::Numeric(kanz_ledger_replay::Error::InvalidRational))
    );
}
#[test]
fn capacity_refusals_preserve_inputs() {
    let empty = BTreeMap::new();
    let mut b = Book::default();
    let huge = Rat::from_integer(BigInt::from(1) << 16383);
    b.positions.entry("equity".into()).or_default().qty = huge;
    let before = b.clone();
    let prices = BTreeMap::from([("equity".into(), Some(n("4")))]);
    assert_eq!(
        valuation::value(&b, "USD", &prices, &BTreeMap::new(), Some(&empty)),
        Err(Error::Numeric(kanz_ledger_replay::Error::Capacity))
    );
    assert_eq!(b, before);
    let b = cash_book(&"x".repeat(257), "1");
    assert_eq!(
        valuation::value(&b, "USD", &empty, &BTreeMap::new(), Some(&empty)),
        Err(Error::Numeric(kanz_ledger_replay::Error::Capacity))
    );
    let prices = (0..=kanz_ledger_replay::MAX_ENTRIES)
        .map(|i| (i.to_string(), Some(n("1"))))
        .collect();
    assert_eq!(
        valuation::value(
            &Book::default(),
            "USD",
            &prices,
            &BTreeMap::new(),
            Some(&empty)
        ),
        Err(Error::Numeric(kanz_ledger_replay::Error::Capacity))
    );
}

#[test]
fn replay_knowledge_cutoff_flows_into_valuation() {
    use kanz_ledger_replay::{replay, Entry, TimePoint};
    let at = |s| TimePoint::new(s, 0).unwrap();
    let cash = Entry {
        id: "cash".into(),
        portfolio: "p".into(),
        kind: 2,
        instrument: String::new(),
        quantity: None,
        price: None,
        cash: Some(n("100")),
        currency: "EUR".into(),
        effective: at(1),
        knowledge: at(1),
        settlement_basis: 1,
        settlement_date: at(1),
        has_action: false,
    };
    let trade = Entry {
        id: "trade".into(),
        kind: 1,
        instrument: "equity".into(),
        quantity: Some(n("2")),
        price: Some(n("10")),
        cash: Some(n("-20")),
        effective: at(2),
        knowledge: at(3),
        ..cash.clone()
    };
    let events = vec![Some(cash), Some(trade)];
    let prices = BTreeMap::from([("equity".into(), Some(n("12")))]);
    let currencies = BTreeMap::from([("equity".into(), "EUR".into())]);
    let rates = BTreeMap::from([("EUR".into(), Some(n("5/4")))]);
    let before = replay("p", &events, Some(at(2)), Some(at(2))).unwrap();
    let after = replay("p", &events, Some(at(2)), Some(at(3))).unwrap();
    // Before learning of the trade: EUR100 -> USD125. After: EUR80 + 2*EUR12 -> USD130.
    assert_eq!(
        valuation::value(&before, "USD", &prices, &currencies, Some(&rates))
            .unwrap()
            .total,
        n("125")
    );
    assert_eq!(
        valuation::value(&after, "USD", &prices, &currencies, Some(&rates))
            .unwrap()
            .total,
        n("130")
    );
}
