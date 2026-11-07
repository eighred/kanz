//! The existing CompleteHistory contract, preserving canonical nested fill wire.
use crate::Error;
use kanz_oms_store::{
    identity,
    recovery_repository::{HISTORY_BYTES, MAX_FILLS},
};
use kanz_order_core::{
    aggregate::ViewState,
    decimal_domain,
    proto::{
        common::v1::Decimal,
        order::v1::{ExecutionRecoveryHistory, Fill, OrderState},
    },
    rational, Canonical,
};
use kanz_proto_wire::{Budget, Schema};
use num_rational::BigRational;
use num_traits::{Signed, Zero};
use prost::Message;
use prost_reflect::DynamicMessage;
use std::collections::BTreeMap;

pub(crate) fn domain<T: Schema>(value: &T) -> Result<(), Error> {
    // Only already bounded generated values enter here. Generated encoding is
    // solely for inspecting known fields; source unknowns remain in Canonical.
    if value.encoded_len() > HISTORY_BYTES {
        return Err(Error::Capacity);
    }
    let dynamic = DynamicMessage::decode(T::descriptor()?, value.encode_to_vec().as_slice())
        .map_err(|_| Error::InvalidInput)?;
    decimal_domain(&dynamic, 0)?;
    Ok(())
}
pub(crate) fn complete(
    state: &Canonical<OrderState>,
    view: ViewState,
    total: Option<&Decimal>,
    fills: Vec<Canonical<Fill>>,
) -> Result<Vec<Canonical<Fill>>, Error> {
    let st = state.value();
    if [
        &st.order_id,
        &st.portfolio_id,
        &st.instrument_id,
        &st.venue,
        &st.venue_account_id,
    ]
    .into_iter()
    .any(|s| s.is_empty())
        || !matches!(st.side, 1 | 2)
        || domain(st).is_err()
    {
        return Err(Error::HistoryIncomplete);
    }
    if matches!(view, ViewState::Unknown | ViewState::Indeterminate) || fills.len() > MAX_FILLS {
        return Err(Error::HistoryIncomplete);
    }
    let total =
        rational(Some(total.ok_or(Error::HistoryIncomplete)?)).ok_or(Error::HistoryIncomplete)?;
    let ordered = rational(st.ordered_quantity.as_ref()).ok_or(Error::HistoryIncomplete)?;
    let filled = rational(st.filled_quantity.as_ref()).ok_or(Error::HistoryIncomplete)?;
    if total.is_negative()
        || total > ordered
        || total < filled
        || (view == ViewState::Filled && total != ordered)
        || (view == ViewState::PartiallyFilled
            && (total <= BigRational::zero() || total >= ordered))
        || (matches!(view, ViewState::Working | ViewState::Rejected) && !total.is_zero())
    {
        return Err(Error::HistoryIncomplete);
    }
    let mut by_execution: BTreeMap<String, usize> = BTreeMap::new();
    let mut by_fill: BTreeMap<String, String> = BTreeMap::new();
    let mut result: Vec<Canonical<Fill>> = Vec::new();
    let mut sum = BigRational::zero();
    for raw in fills {
        let f = raw.value();
        let quantity = rational(f.quantity.as_ref()).ok_or(Error::HistoryIncomplete)?;
        let price = rational(f.price.as_ref()).ok_or(Error::HistoryIncomplete)?;
        let valid_time = f.executed_at.as_ref().is_some_and(|t| {
            t.seconds > 0 && t.seconds <= 253402300799 && (0..1_000_000_000).contains(&t.nanos)
        });
        if domain(f).is_err()
            || f.fill_id.is_empty()
            || f.venue.is_empty()
            || !quantity.is_positive()
            || f.order_id != st.order_id
            || f.instrument_id != st.instrument_id
            || f.venue != st.venue
            || f.venue_account_id != st.venue_account_id
            || f.side != st.side
            || f.venue_execution_id.is_empty()
            || f.venue_execution_id.len() > 256
            || f.fill_id.len() > 512
            || f.recovery.is_some()
            || f.price.is_none()
            || !price.is_positive()
            || !f
                .fee
                .as_ref()
                .is_some_and(|fee| fee.amount.is_some() && !fee.currency_code.is_empty())
            || !valid_time
        {
            return Err(Error::HistoryIncomplete);
        }
        if let Some(i) = by_execution.get(&f.venue_execution_id) {
            if !identity::same_execution(result[*i].value(), f) {
                return Err(Error::HistoryIncomplete);
            }
            continue;
        }
        if by_fill
            .get(&f.fill_id)
            .is_some_and(|previous| previous != &f.venue_execution_id)
        {
            return Err(Error::HistoryIncomplete);
        }
        by_execution.insert(f.venue_execution_id.clone(), result.len());
        by_fill.insert(f.fill_id.clone(), f.venue_execution_id.clone());
        sum += quantity;
        result.push(raw);
    }
    if sum != total {
        return Err(Error::HistoryIncomplete);
    }
    result.sort_by(|a, b| {
        let (a, b) = (a.value(), b.value());
        let (at, bt) = (
            a.executed_at.as_ref().expect("validated"),
            b.executed_at.as_ref().expect("validated"),
        );
        (at.seconds, at.nanos, &a.venue_execution_id).cmp(&(
            bt.seconds,
            bt.nanos,
            &b.venue_execution_id,
        ))
    });
    Ok(result)
}
fn append_message(out: &mut Vec<u8>, number: u32, raw: &[u8]) -> Result<(), Error> {
    kanz_proto_wire::append_message(number, raw, out, Budget::RecoveryHistory).map_err(Error::from)
}

/// Canonical history is immutable; revalidate completeness against the current
/// order on restart without consulting a new venue response.
pub(crate) fn from_frozen(
    state: &Canonical<OrderState>,
    history: &Canonical<ExecutionRecoveryHistory>,
) -> Result<Vec<Canonical<Fill>>, Error> {
    let h = history.value();
    if h.order_id != state.value().order_id || h.fills.len() > MAX_FILLS {
        return Err(Error::HistoryIncomplete);
    }
    complete(
        state,
        ViewState::Cancelled,
        h.executed_quantity.as_ref(),
        frozen_fills(history)?,
    )
}
/// Extract immutable nested wire without imposing a new proposal-time economic
/// check. CommitRecovery separately revalidates complete history against state.
pub(crate) fn frozen_fills(
    history: &Canonical<ExecutionRecoveryHistory>,
) -> Result<Vec<Canonical<Fill>>, Error> {
    let descriptor = ExecutionRecoveryHistory::descriptor()?;
    let number = descriptor
        .get_field_by_name("fills")
        .ok_or(Error::InvalidInput)?
        .number();
    let mut pos = 0;
    let mut fills = Vec::new();
    while pos < history.wire().len() {
        let (n, w) = kanz_proto_wire::key(history.wire(), &mut pos)?;
        if n == number && w == 2 {
            if fills.len() >= MAX_FILLS {
                return Err(Error::Capacity);
            }
            let raw = kanz_proto_wire::payload(history.wire(), &mut pos)?;
            fills.push(Canonical::decode_with_budget(raw, Budget::RecoveryHistory)?);
        } else {
            kanz_proto_wire::skip(history.wire(), &mut pos, n, w)?;
        }
    }
    Ok(fills)
}
pub(crate) fn freeze_wire(
    order: &str,
    mapping: &str,
    fills: &[Canonical<Fill>],
    quantity: &[u8],
    at: prost_types::Timestamp,
) -> Result<Canonical<ExecutionRecoveryHistory>, Error> {
    let h = ExecutionRecoveryHistory {
        order_id: order.into(),
        mapping_version: mapping.into(),
        observed_at: Some(at),
        ..Default::default()
    };
    let mut raw = h.encode_to_vec();
    for fill in fills {
        append_message(&mut raw, 3, fill.wire())?;
    }
    append_message(&mut raw, 4, quantity)?;
    Ok(Canonical::decode_with_budget(
        &raw,
        Budget::RecoveryHistory,
    )?)
}
#[cfg(test)]
mod tests {
    use super::*;
    use kanz_order_core::proto::common::v1::Money;
    fn d(c: i64, e: i32) -> Decimal {
        Decimal {
            coefficient: c,
            exponent: e,
        }
    }
    fn state() -> Canonical<OrderState> {
        Canonical::from_value(OrderState {
            order_id: "o".into(),
            portfolio_id: "p".into(),
            instrument_id: "i".into(),
            venue: "MIC".into(),
            venue_account_id: "a".into(),
            side: 1,
            ordered_quantity: Some(d(1, 0)),
            filled_quantity: Some(d(0, 0)),
            ..Default::default()
        })
        .unwrap()
    }
    fn fill(id: &str, q: i64, time: i64) -> Canonical<Fill> {
        Canonical::from_value(Fill {
            fill_id: id.into(),
            venue_execution_id: id.into(),
            order_id: "o".into(),
            instrument_id: "i".into(),
            venue: "MIC".into(),
            venue_account_id: "a".into(),
            side: 1,
            quantity: Some(d(q, -1)),
            price: Some(d(17, 0)),
            fee: Some(Money {
                amount: Some(d(0, 0)),
                currency_code: "USD".into(),
            }),
            executed_at: Some(prost_types::Timestamp {
                seconds: time,
                nanos: 0,
            }),
            ..Default::default()
        })
        .unwrap()
    }
    #[test]
    fn exact_unique_quantity_and_economic_time_order() {
        let a = fill("a", 3, 2);
        let b = fill("b", 7, 1);
        let result = complete(
            &state(),
            ViewState::Filled,
            Some(&d(1, 0)),
            vec![a.clone(), b, a],
        )
        .unwrap();
        assert_eq!(
            result
                .iter()
                .map(|f| f.value().fill_id.as_str())
                .collect::<Vec<_>>(),
            vec!["b", "a"]
        );
        assert!(complete(
            &state(),
            ViewState::Filled,
            Some(&d(1, 0)),
            vec![fill("a", 3, 2), fill("b", 6, 1)]
        )
        .is_err());
    }
    #[test]
    fn missing_independent_total_and_conflicting_duplicate_refuse_whole_history() {
        assert!(complete(&state(), ViewState::Cancelled, None, vec![]).is_err());
        assert!(complete(
            &state(),
            ViewState::Filled,
            Some(&d(1, 0)),
            vec![fill("a", 3, 1), fill("a", 7, 1)]
        )
        .is_err());
        assert!(complete(&state(), ViewState::Unknown, Some(&d(0, 0)), vec![]).is_err());
    }
}
