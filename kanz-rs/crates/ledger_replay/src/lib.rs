#![forbid(unsafe_code)]
//! Pure, bounded accounting posting/replay candidate for #1535. No authority,
//! store receipt, wire codec, tenant authorization or corporate-action engine.
//! Caller supplies an already authorized portfolio journal; Go remains owner.
use kanz_order_core::cost_basis::{bound, Lot};
use num_rational::BigRational as Rat;
use num_traits::Zero;
use std::collections::{BTreeMap, BTreeSet};

pub mod valuation;

pub const MAX_ENTRIES: usize = 65_536;
const MAX_KEY_BYTES: usize = 256;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Error {
    Capacity,
    InvalidTime,
    InvalidRational,
    Unsupported,
}
impl From<kanz_order_core::cost_basis::Error> for Error {
    fn from(value: kanz_order_core::cost_basis::Error) -> Self {
        match value {
            kanz_order_core::cost_basis::Error::Capacity => Self::Capacity,
            kanz_order_core::cost_basis::Error::InvalidRational => Self::InvalidRational,
        }
    }
}

/// UTC seconds plus nanoseconds, avoiding UnixNano overflow outside 1678–2262.
/// The Go zero instant is an unbounded read cutoff, never a default to now.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub struct TimePoint {
    seconds: i64,
    nanos: u32,
}
impl TimePoint {
    pub const ZERO: Self = Self {
        seconds: -62_135_596_800,
        nanos: 0,
    };
    pub fn new(seconds: i64, nanos: u32) -> Result<Self, Error> {
        if !(-62_135_596_800..=253_402_300_799).contains(&seconds) || nanos >= 1_000_000_000 {
            return Err(Error::InvalidTime);
        }
        Ok(Self { seconds, nanos })
    }
}

/// Internal diagnostic capture of the Go Event's fold inputs. This is not a
/// second LedgerEntry wire contract: the canonical proto omits settlement basis.
/// Opaque execution evidence, source refs and store receipts are never rewritten.
#[derive(Clone, Debug)]
pub struct Entry {
    pub id: String,
    pub portfolio: String,
    pub kind: i32,
    pub instrument: String,
    pub quantity: Option<Rat>,
    pub price: Option<Rat>,
    pub cash: Option<Rat>,
    pub currency: String,
    pub effective: TimePoint,
    pub knowledge: TimePoint,
    pub settlement_basis: i32,
    pub settlement_date: TimePoint,
    /// True for any Go Event.Action, including cancelled/revised actions.
    pub has_action: bool,
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Book {
    pub positions: BTreeMap<String, Lot>,
    pub cash: BTreeMap<String, Rat>,
    pub accrued: BTreeMap<String, Rat>,
    pub settled_positions: BTreeMap<String, Lot>,
    pub settled_cash: BTreeMap<String, Rat>,
    pub unknown_settlement: usize,
    pub pending_settlement: usize,
}
impl Book {
    /// Fresh replay only; legacy checkpoint restoration is not implemented.
    pub fn settlement_basis_complete(&self) -> bool {
        self.unknown_settlement == 0
    }
}

fn add(map: &mut BTreeMap<String, Rat>, key: &str, value: &Rat) -> Result<(), Error> {
    let next = map.entry(key.to_owned()).or_default();
    *next += value;
    bound(next).map_err(Error::from)
}

/// Reconstruct ordinary trade/cash/fee/accrual postings exactly, in stable
/// (effective, knowledge, entry-id) order. Equal keys retain journal input order.
/// Unknown settlement is counted even for an empty entry. Dates alone never
/// settle cash. Nil legs and zero-valued map entries follow the Go fold.
///
/// Corporate actions require revision resolution before effective filtering;
/// refuse any known relevant action before applying that cutoff. No partial
/// book escapes on unsupported/capacity errors. None/Go-zero cutoffs are unbounded.
pub fn replay(
    portfolio: &str,
    entries: &[Option<Entry>],
    effective: Option<TimePoint>,
    knowledge: Option<TimePoint>,
) -> Result<Book, Error> {
    if entries.len() > MAX_ENTRIES || portfolio.len() > MAX_KEY_BYTES {
        return Err(Error::Capacity);
    }
    let effective = effective.filter(|t| *t != TimePoint::ZERO);
    let knowledge = knowledge.filter(|t| *t != TimePoint::ZERO);
    let mut ordered = Vec::new();
    for entry in entries.iter().flatten() {
        if (!entry.portfolio.is_empty() && entry.portfolio != portfolio)
            || knowledge.is_some_and(|cut| entry.knowledge > cut)
        {
            continue;
        }
        if entry.has_action || !matches!(entry.kind, 0..=3 | 5) {
            return Err(Error::Unsupported);
        }
        if effective.is_some_and(|cut| entry.effective > cut) || entry.id.is_empty() {
            continue;
        }
        if [
            &entry.id,
            &entry.portfolio,
            &entry.instrument,
            &entry.currency,
        ]
        .iter()
        .any(|s| s.len() > MAX_KEY_BYTES)
        {
            return Err(Error::Capacity);
        }
        for value in [&entry.quantity, &entry.price, &entry.cash]
            .into_iter()
            .flatten()
        {
            bound(value)?;
        }
        ordered.push(entry);
    }
    ordered
        .sort_by(|a, b| (a.effective, a.knowledge, &a.id).cmp(&(b.effective, b.knowledge, &b.id)));
    let mut seen = BTreeSet::new();
    let mut book = Book::default();
    for entry in ordered {
        if !seen.insert(&entry.id) {
            continue;
        }
        let settled = entry.settlement_basis == 1;
        match entry.settlement_basis {
            1 => {}
            2 => book.pending_settlement += 1,
            _ => book.unknown_settlement += 1,
        }
        if let Some(quantity) = entry
            .quantity
            .as_ref()
            .filter(|q| !q.is_zero() && !entry.instrument.is_empty())
        {
            let zero = Rat::zero();
            let price = entry.price.as_ref().unwrap_or(&zero);
            book.positions
                .entry(entry.instrument.clone())
                .or_default()
                .fold(quantity, price)?;
            if settled {
                book.settled_positions
                    .entry(entry.instrument.clone())
                    .or_default()
                    .fold(quantity, price)?;
            }
        }
        if let Some(cash) = entry.cash.as_ref().filter(|v| !v.is_zero()) {
            if entry.kind == 5 {
                add(&mut book.accrued, &entry.currency, cash)?;
            } else {
                add(&mut book.cash, &entry.currency, cash)?;
                if settled {
                    add(&mut book.settled_cash, &entry.currency, cash)?;
                }
            }
        }
    }
    Ok(book)
}
