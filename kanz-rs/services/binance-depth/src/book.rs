//! Single-owner exact book fold. Invalid updates leave the previous book intact.
use crate::decimal::{rational, scaled};
use crate::proto::market::v1::{OrderBookDelta, OrderBookSnapshot, PriceLevel};
use num_rational::BigRational;
use num_traits::Zero;
use std::collections::BTreeMap;

type Side = BTreeMap<BigRational, BigRational>;
pub const MAX_LEVELS_PER_SIDE: usize = 100_000;
#[derive(Debug, PartialEq, Eq)]
pub enum BookError {
    NoVenueTime,
    SequenceGap,
    DecimalDomain,
    Capacity,
}

pub struct Book {
    metadata: OrderBookSnapshot,
    bids: Side,
    asks: Side,
    seeded: bool,
}
impl Book {
    pub fn new(instrument_id: String, symbol: String, mic: String) -> Self {
        Self {
            metadata: OrderBookSnapshot {
                instrument_id,
                symbol,
                mic,
                ..Default::default()
            },
            bids: Side::new(),
            asks: Side::new(),
            seeded: false,
        }
    }
    pub fn apply_snapshot(&mut self, snapshot: &OrderBookSnapshot) -> Result<(), BookError> {
        if snapshot.event_time.is_none() {
            return Err(BookError::NoVenueTime);
        }
        let bids = decoded(&snapshot.bids)?;
        let asks = decoded(&snapshot.asks)?;
        capacity(&Side::new(), &bids, true, MAX_LEVELS_PER_SIDE)?;
        capacity(&Side::new(), &asks, true, MAX_LEVELS_PER_SIDE)?;
        self.bids.clear();
        self.asks.clear();
        // Snapshot ignores nonpositive rows (including later duplicate rows),
        // whereas a delta deletes them: preserve that Go distinction.
        for (price, size) in bids {
            if size > BigRational::zero() {
                self.bids.insert(price, size);
            }
        }
        for (price, size) in asks {
            if size > BigRational::zero() {
                self.asks.insert(price, size);
            }
        }
        self.metadata.event_time = snapshot.event_time;
        self.metadata.last_update_sequence = snapshot.last_update_sequence;
        self.seeded = true;
        Ok(())
    }
    pub fn apply_delta(&mut self, delta: &OrderBookDelta) -> Result<(), BookError> {
        if self.seeded
            && self.metadata.last_update_sequence != 0
            && delta.prev_update_sequence != 0
            && delta.prev_update_sequence != self.metadata.last_update_sequence
        {
            return Err(BookError::SequenceGap);
        }
        if !self.seeded && delta.event_time.is_none() {
            return Err(BookError::NoVenueTime);
        }
        let bids = decoded(&delta.bids)?;
        let asks = decoded(&delta.asks)?;
        capacity(&self.bids, &bids, false, MAX_LEVELS_PER_SIDE)?;
        capacity(&self.asks, &asks, false, MAX_LEVELS_PER_SIDE)?;
        apply(&mut self.bids, bids);
        apply(&mut self.asks, asks);
        if delta.last_update_sequence != 0 {
            self.metadata.last_update_sequence = delta.last_update_sequence;
        }
        if delta.event_time.is_some() {
            self.metadata.event_time = delta.event_time;
        }
        self.seeded = true;
        Ok(())
    }
    /// Zero means all levels; positive depth caps each side independently.
    pub fn snapshot(&self, depth: usize) -> OrderBookSnapshot {
        let mut out = self.metadata.clone();
        out.bids = levels(self.bids.iter().rev(), depth);
        out.asks = levels(self.asks.iter(), depth);
        out
    }
}
// Track only touched keys, not a clone of the resident book. Last update wins;
// snapshot nonpositive rows are ignored rather than deleting earlier rows.
fn capacity(
    side: &Side,
    rows: &[(BigRational, BigRational)],
    snapshot: bool,
    limit: usize,
) -> Result<(), BookError> {
    let mut final_presence = BTreeMap::new();
    for (price, size) in rows {
        let present = size > &BigRational::zero();
        if !snapshot || present {
            final_presence.insert(price, present);
        }
    }
    let mut count = side.len();
    for (price, present) in final_presence {
        match (side.contains_key(price), present) {
            (true, false) => count -= 1,
            (false, true) => count += 1,
            _ => {}
        }
    }
    if count > limit {
        Err(BookError::Capacity)
    } else {
        Ok(())
    }
}
fn decoded(rows: &[PriceLevel]) -> Result<Vec<(BigRational, BigRational)>, BookError> {
    rows.iter()
        .map(|row| {
            Ok((
                rational(row.price.as_ref()).ok_or(BookError::DecimalDomain)?,
                rational(row.size.as_ref()).ok_or(BookError::DecimalDomain)?,
            ))
        })
        .collect()
}
fn apply(side: &mut Side, rows: Vec<(BigRational, BigRational)>) {
    for (price, size) in rows {
        if size <= BigRational::zero() {
            side.remove(&price);
        } else {
            side.insert(price, size);
        }
    }
}
fn levels<'a>(
    rows: impl Iterator<Item = (&'a BigRational, &'a BigRational)>,
    depth: usize,
) -> Vec<PriceLevel> {
    rows.take(if depth == 0 { usize::MAX } else { depth })
        .filter_map(|(price, size)| {
            Some(PriceLevel {
                price: Some(scaled(price)?),
                size: Some(scaled(size)?),
            })
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::decimal::parse;
    fn row(price: &str, size: &str) -> PriceLevel {
        PriceLevel {
            price: parse(price),
            size: parse(size),
        }
    }
    fn seed(rows: Vec<PriceLevel>) -> OrderBookSnapshot {
        OrderBookSnapshot {
            bids: rows,
            event_time: Some(Default::default()),
            last_update_sequence: 10,
            ..Default::default()
        }
    }
    fn book() -> Book {
        Book::new("i".into(), "BTCUSDT".into(), "BINANCE".into())
    }
    #[test]
    fn equivalent_keys_duplicates_numeric_order_and_caps() {
        let mut b = book();
        b.apply_snapshot(&seed(vec![
            row("2", "1"),
            row("10", "1"),
            row("100.00", "2"),
            row("1e2", "3"),
            row("100", "0"),
        ]))
        .unwrap();
        let s = b.snapshot(2);
        assert_eq!(s.bids, vec![row("100", "3"), row("10", "1")]);
        b.apply_delta(&OrderBookDelta {
            bids: vec![row("100", "0.0000000049"), row("10", "-1")],
            prev_update_sequence: 10,
            last_update_sequence: 11,
            ..Default::default()
        })
        .unwrap();
        assert_eq!(b.snapshot(0).bids, vec![row("2", "1")]);
    }
    #[test]
    fn rejects_before_mutating_and_preserves_venue_time() {
        let mut b = book();
        assert_eq!(
            b.apply_snapshot(&OrderBookSnapshot::default()),
            Err(BookError::NoVenueTime)
        );
        assert_eq!(
            b.apply_delta(&OrderBookDelta::default()),
            Err(BookError::NoVenueTime)
        );
        b.apply_snapshot(&seed(vec![row("100", "2")])).unwrap();
        let before = b.snapshot(0);
        assert_eq!(
            b.apply_delta(&OrderBookDelta {
                prev_update_sequence: 9,
                bids: vec![row("100", "0")],
                ..Default::default()
            }),
            Err(BookError::SequenceGap)
        );
        assert_eq!(b.snapshot(0), before);
        let mut invalid = row("100", "0");
        invalid.price.as_mut().unwrap().exponent = i32::MAX;
        assert_eq!(
            b.apply_delta(&OrderBookDelta {
                bids: vec![row("100", "3"), invalid],
                ..Default::default()
            }),
            Err(BookError::DecimalDomain)
        );
        assert_eq!(b.snapshot(0), before);
    }
    #[test]
    fn capacity_counts_final_unique_keys() {
        let side: Side = [(
            BigRational::from_integer(1.into()),
            BigRational::from_integer(1.into()),
        )]
        .into();
        let rows = decoded(&[row("2", "1"), row("2.0", "2"), row("1", "0")]).unwrap();
        assert_eq!(capacity(&side, &rows, false, 1), Ok(()));
        assert_eq!(
            capacity(
                &Side::new(),
                &decoded(&[row("1", "1"), row("1", "0")]).unwrap(),
                true,
                0
            ),
            Err(BookError::Capacity)
        );
    }
    #[test]
    fn capacity_refusal_is_atomic_across_sides() {
        let mut b = book();
        b.apply_snapshot(&seed(vec![row("1", "1")])).unwrap();
        // Fill resident state directly: exercise the production boundary without
        // spending test time parsing 100,000 redundant decimal strings.
        for i in 1..=MAX_LEVELS_PER_SIDE {
            b.asks.insert(
                BigRational::from_integer(i.into()),
                BigRational::from_integer(1.into()),
            );
        }
        let before = b.snapshot(1);
        let delta = OrderBookDelta {
            bids: vec![row("1", "0")],
            asks: vec![row("100001", "1")],
            last_update_sequence: 11,
            ..Default::default()
        };
        assert_eq!(b.apply_delta(&delta), Err(BookError::Capacity));
        assert_eq!(b.snapshot(1), before);
        assert_eq!(b.asks.len(), MAX_LEVELS_PER_SIDE);
        let too_many = (0..=MAX_LEVELS_PER_SIDE)
            .map(|i| PriceLevel {
                price: Some(crate::proto::common::v1::Decimal {
                    coefficient: i as i64,
                    exponent: 0,
                }),
                size: parse("1"),
            })
            .collect();
        assert_eq!(b.apply_snapshot(&seed(too_many)), Err(BookError::Capacity));
        assert_eq!(b.snapshot(1), before);
    }
}
