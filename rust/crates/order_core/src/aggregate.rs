//! Existing Go aggregate rules. Admission, attribution and durable effects remain callers' owners.
use crate::proto::order::v1::{AmendOrder, Fill, OrderState, OrderStatus, SubmitOrder};
use crate::{decimal, rational, Canonical};
use kanz_proto_wire::{Schema, MAX_BYTES};
use num_traits::Zero;
use prost::Message;
use prost_types::Timestamp;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    Rejected(&'static str),
    UnsupportedInput,
    NumericalFailure,
    Capacity,
}
impl From<kanz_proto_wire::Error> for Error {
    fn from(value: kanz_proto_wire::Error) -> Self {
        match value {
            kanz_proto_wire::Error::Capacity => Self::Capacity,
            kanz_proto_wire::Error::InvalidInput => Self::UnsupportedInput,
        }
    }
}
fn time(now: &Timestamp) -> Result<(), Error> {
    if (-62_135_596_800..=253_402_300_799).contains(&now.seconds)
        && (0..1_000_000_000).contains(&now.nanos)
    {
        Ok(())
    } else {
        Err(Error::UnsupportedInput)
    }
}
pub fn is_terminal(st: &OrderState) -> bool {
    matches!(
        OrderStatus::try_from(st.status),
        Ok(OrderStatus::Filled
            | OrderStatus::Cancelled
            | OrderStatus::Rejected
            | OrderStatus::Expired)
    )
}
fn working(st: &OrderState) -> Result<(), Error> {
    if is_terminal(st) {
        Err(Error::Rejected("ORDER_TERMINAL"))
    } else {
        Ok(())
    }
}
fn number<T: Schema>(name: &str) -> Result<u32, Error> {
    T::descriptor()?
        .get_field_by_name(name)
        .map(|f| f.number())
        .ok_or(Error::UnsupportedInput)
}
fn append(out: &mut Vec<u8>, bytes: &[u8]) -> Result<(), Error> {
    if out
        .len()
        .checked_add(bytes.len())
        .is_none_or(|n| n > MAX_BYTES)
    {
        return Err(Error::Capacity);
    }
    out.extend_from_slice(bytes);
    Ok(())
}
fn fields(raw: &[u8], keep: impl Fn(u32, u8) -> bool) -> Result<Vec<u8>, Error> {
    let mut out = Vec::new();
    let mut pos = 0;
    while pos < raw.len() {
        let start = pos;
        let (n, w) = kanz_proto_wire::key(raw, &mut pos)?;
        kanz_proto_wire::skip(raw, &mut pos, n, w)?;
        if keep(n, w) {
            append(&mut out, &raw[start..pos])?;
        }
    }
    Ok(out)
}
fn patch(
    st: &Canonical<OrderState>,
    candidate: OrderState,
    names: &[&str],
) -> Result<Canonical<OrderState>, Error> {
    let numbers: Vec<u32> = names
        .iter()
        .map(|n| number::<OrderState>(n))
        .collect::<Result<_, _>>()?;
    let descriptor = OrderState::descriptor()?;
    let changed_field = |n, w| {
        numbers.contains(&n)
            && descriptor
                .get_field(n)
                .is_some_and(|f| kanz_proto_wire::accepts_wire(&f, w))
    };
    let mut raw = fields(st.wire(), |n, w| !changed_field(n, w))?;
    let changed = fields(&candidate.encode_to_vec(), changed_field)?;
    append(&mut raw, &changed)?;
    Ok(Canonical::decode(&raw)?)
}

/// Existing verified-history projection. This pure operation is not evidence
/// that history is complete or permitted: the recovery coordinator owns that
/// mandatory validation before calling it and persists refusal before ACK.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RecoveryFoldError {
    Evidence,
    PostedUnknown,
    Fold(Error),
    Representation(Error),
}
pub fn apply_recovered_fill(
    st: &Canonical<OrderState>,
    fill: &Canonical<Fill>,
    now: Timestamp,
) -> Result<Canonical<OrderState>, RecoveryFoldError> {
    if fill.value().recovery.is_none() {
        return Err(RecoveryFoldError::Evidence);
    }
    time(&now).map_err(RecoveryFoldError::Representation)?;
    let terminal = is_terminal(st.value());
    let mut working = st.value().clone();
    if terminal {
        working.status = OrderStatus::Routed as i32;
    }
    let leaves = rational(working.ordered_quantity.as_ref())
        .ok_or(RecoveryFoldError::PostedUnknown)?
        - rational(working.filled_quantity.as_ref()).ok_or(RecoveryFoldError::PostedUnknown)?;
    working.leaves_quantity = Some(decimal(leaves).ok_or(RecoveryFoldError::PostedUnknown)?);
    let working = patch(st, working, &["status", "leaves_quantity"])
        .map_err(RecoveryFoldError::Representation)?;
    // Only failure of the existing ApplyFill fold enters Go's durable BlockRecovery branch.
    let next = apply_fill(&working, fill, now).map_err(RecoveryFoldError::Fold)?;
    let mut value = next.value().clone();
    if terminal {
        value.status = st.value().status;
    }
    value.as_of = Some(now);
    patch(&next, value, &["status", "as_of"]).map_err(RecoveryFoldError::Representation)
}

/// One final round from the verified complete journal, never a comparison of
/// exact history against previously rounded incremental averages.
pub fn recovery_average(
    st: &Canonical<OrderState>,
    cost: num_rational::BigRational,
    total: &crate::proto::common::v1::Decimal,
) -> Result<Canonical<OrderState>, Error> {
    let total = rational(Some(total)).ok_or(Error::UnsupportedInput)?;
    if total <= num_rational::BigRational::zero() {
        return Ok(st.clone());
    }
    let mut value = st.value().clone();
    value.average_fill_price = Some(decimal(cost / total).ok_or(Error::NumericalFailure)?);
    patch(st, value, &["average_fill_price"])
}
fn carry<A: Schema>(
    target: Canonical<OrderState>,
    source: &Canonical<A>,
    from: &str,
    to: &str,
) -> Result<Canonical<OrderState>, Error> {
    let source_number = number::<A>(from)?;
    let target_number = number::<OrderState>(to)?;
    let mut raw = fields(target.wire(), |n, w| n != target_number || w != 2)?;
    let mut pos = 0;
    while pos < source.wire().len() {
        let (n, w) = kanz_proto_wire::key(source.wire(), &mut pos)?;
        if n == source_number && w == 2 {
            let message = kanz_proto_wire::payload(source.wire(), &mut pos)?;
            if raw
                .len()
                .checked_add(message.len() + 10)
                .is_none_or(|n| n > MAX_BYTES)
            {
                return Err(Error::Capacity);
            }
            kanz_proto_wire::put_message(target_number, message, &mut raw);
        } else {
            kanz_proto_wire::skip(source.wire(), &mut pos, n, w)?;
        }
    }
    Ok(Canonical::decode(&raw)?)
}

pub fn accept(
    cmd: &Canonical<SubmitOrder>,
    now: Timestamp,
) -> Result<Canonical<OrderState>, Error> {
    use crate::proto::order::v1::{OrderType, Side, TimeInForce};
    time(&now)?;
    let c = cmd.value();
    if c.order_id.is_empty()
        || c.portfolio_id.is_empty()
        || c.instrument_id.is_empty()
        || c.instrument_id.len() > 64
        || c.instrument_id.bytes().any(|b| b < 0x20 || b == 0x7f)
        || c.side == Side::Unspecified as i32
    {
        return Err(Error::Rejected("INVALID_ORDER"));
    }
    if c.quantity.as_ref().is_none_or(|q| q.coefficient <= 0) {
        return Err(Error::Rejected("INVALID_QUANTITY"));
    }
    if c.time_in_force == TimeInForce::Unspecified as i32
        || (c.time_in_force == TimeInForce::Gtd as i32 && c.expire_at.is_none())
        || c.order_type == OrderType::Unspecified as i32
    {
        return Err(Error::Rejected("INVALID_ORDER"));
    }
    if matches!(
        OrderType::try_from(c.order_type),
        Ok(OrderType::Limit | OrderType::StopLimit)
    ) && c.limit_price.as_ref().is_none_or(|p| p.coefficient <= 0)
    {
        return Err(Error::Rejected("INVALID_PRICE"));
    }
    if matches!(
        OrderType::try_from(c.order_type),
        Ok(OrderType::Stop | OrderType::StopLimit)
    ) && c.stop_price.as_ref().is_none_or(|p| p.coefficient <= 0)
    {
        return Err(Error::Rejected("INVALID_PRICE"));
    }
    let mut state = Canonical::from_value(OrderState {
        order_id: c.order_id.clone(),
        portfolio_id: c.portfolio_id.clone(),
        instrument_id: c.instrument_id.clone(),
        side: c.side,
        order_type: c.order_type,
        time_in_force: c.time_in_force,
        ordered_quantity: c.quantity,
        leaves_quantity: c.quantity,
        filled_quantity: Some(crate::proto::common::v1::Decimal {
            coefficient: 0,
            exponent: 0,
        }),
        limit_price: c.limit_price,
        stop_price: c.stop_price,
        expire_at: c.expire_at,
        margin_mode: c.margin_mode,
        leverage: c.leverage,
        venue: c.venue.clone(),
        status: OrderStatus::PendingNew as i32,
        as_of: Some(now),
        ..Default::default()
    })?;
    for (from, to) in [
        ("quantity", "ordered_quantity"),
        ("quantity", "leaves_quantity"),
        ("limit_price", "limit_price"),
        ("stop_price", "stop_price"),
        ("expire_at", "expire_at"),
        ("leverage", "leverage"),
    ] {
        state = carry(state, cmd, from, to)?;
    }
    Ok(state)
}

/// Generated-value compatibility seam for the existing diagnostic wrapper.
/// Wire consumers use the canonical transition APIs to retain unknown metadata.
pub fn transition_values(
    st: &OrderState,
    target: OrderStatus,
    now: Timestamp,
) -> Result<OrderState, Error> {
    if st.encoded_len() > MAX_BYTES {
        return Err(Error::Capacity);
    }
    working(st)?;
    time(&now)?;
    if !matches!(
        target,
        OrderStatus::Routed | OrderStatus::Rejected | OrderStatus::Cancelled | OrderStatus::Expired
    ) {
        return Err(Error::UnsupportedInput);
    }
    let mut next = st.clone();
    next.status = target as i32;
    next.as_of = Some(now);
    Ok(next)
}
fn transition(
    st: &Canonical<OrderState>,
    target: OrderStatus,
    now: Timestamp,
) -> Result<Canonical<OrderState>, Error> {
    patch(
        st,
        transition_values(st.value(), target, now)?,
        &["status", "as_of"],
    )
}
pub fn route(st: &Canonical<OrderState>, now: Timestamp) -> Result<Canonical<OrderState>, Error> {
    transition(st, OrderStatus::Routed, now)
}
pub fn reject(st: &Canonical<OrderState>, now: Timestamp) -> Result<Canonical<OrderState>, Error> {
    transition(st, OrderStatus::Rejected, now)
}
/// Leaves remain present/absent and byte-exact in the result; no zeroing is implied.
pub fn cancel(st: &Canonical<OrderState>, now: Timestamp) -> Result<Canonical<OrderState>, Error> {
    transition(st, OrderStatus::Cancelled, now)
}
pub fn expire(st: &Canonical<OrderState>, now: Timestamp) -> Result<Canonical<OrderState>, Error> {
    transition(st, OrderStatus::Expired, now)
}

/// Shared numerical fold. Wire consumers use `apply_fill` for metadata retention.
pub fn apply_fill_values(
    st: &OrderState,
    fill: &Fill,
    now: Timestamp,
) -> Result<OrderState, Error> {
    if st.encoded_len() > MAX_BYTES {
        return Err(Error::Capacity);
    }
    working(st)?;
    time(&now)?;
    if fill.quantity.as_ref().is_none_or(|q| q.coefficient <= 0) {
        return Err(Error::Rejected("INVALID_FILL"));
    }
    let (q, leaves, old, ordered, avg, price) = (
        rational(fill.quantity.as_ref()),
        rational(st.leaves_quantity.as_ref()),
        rational(st.filled_quantity.as_ref()),
        rational(st.ordered_quantity.as_ref()),
        rational(st.average_fill_price.as_ref()),
        rational(fill.price.as_ref()),
    );
    let (Some(q), Some(leaves), Some(old), Some(ordered), Some(avg), Some(price)) =
        (q, leaves, old, ordered, avg, price)
    else {
        return Err(Error::UnsupportedInput);
    };
    if q > leaves {
        return Err(Error::Rejected("OVERFILL"));
    }
    let filled = &old + &q;
    let remaining = ordered - &filled;
    if filled.is_zero() {
        return Err(Error::NumericalFailure);
    }
    let weighted = (old * avg + q * price) / &filled;
    let (Some(filled_wire), Some(leaves_wire), Some(avg_wire)) = (
        decimal(filled),
        decimal(remaining.clone()),
        decimal(weighted),
    ) else {
        return Err(Error::NumericalFailure);
    };
    let mut next = st.clone();
    next.filled_quantity = Some(filled_wire);
    next.leaves_quantity = Some(leaves_wire);
    next.average_fill_price = Some(avg_wire);
    next.as_of = Some(now);
    next.status = if remaining.is_zero() {
        OrderStatus::Filled as i32
    } else {
        OrderStatus::PartiallyFilled as i32
    };
    Ok(next)
}
pub fn apply_fill(
    st: &Canonical<OrderState>,
    fill: &Canonical<Fill>,
    now: Timestamp,
) -> Result<Canonical<OrderState>, Error> {
    patch(
        st,
        apply_fill_values(st.value(), fill.value(), now)?,
        &[
            "filled_quantity",
            "leaves_quantity",
            "average_fill_price",
            "status",
            "as_of",
        ],
    )
}
pub fn amend(
    st: &Canonical<OrderState>,
    cmd: &Canonical<AmendOrder>,
    now: Timestamp,
) -> Result<Canonical<OrderState>, Error> {
    use crate::proto::order::v1::OrderType;
    working(st.value())?;
    time(&now)?;
    let c = cmd.value();
    let mut next = st.value().clone();
    let mut names = vec!["as_of"];
    if let Some(q) = c.new_quantity {
        if q.coefficient <= 0 {
            return Err(Error::Rejected("INVALID_QUANTITY"));
        }
        let q_value = rational(Some(&q)).ok_or(Error::UnsupportedInput)?;
        let filled =
            rational(st.value().filled_quantity.as_ref()).ok_or(Error::UnsupportedInput)?;
        if q_value < filled {
            return Err(Error::Rejected("INVALID_QUANTITY"));
        }
        next.ordered_quantity = Some(q);
        next.leaves_quantity = Some(decimal(q_value - filled).ok_or(Error::NumericalFailure)?);
        names.extend(["ordered_quantity", "leaves_quantity"]);
    }
    if let Some(p) = c.new_limit_price {
        if !matches!(
            OrderType::try_from(st.value().order_type),
            Ok(OrderType::Limit | OrderType::StopLimit)
        ) {
            return Err(Error::Rejected("INVALID_PRICE"));
        }
        // Existing aggregate assigns it; mandatory caller admission owns price policy.
        next.limit_price = Some(p);
        names.push("limit_price");
    }
    next.as_of = Some(now);
    let mut out = patch(st, next, &names)?;
    if c.new_quantity.is_some() {
        out = carry(out, cmd, "new_quantity", "ordered_quantity")?;
    }
    if c.new_limit_price.is_some() {
        out = carry(out, cmd, "new_limit_price", "limit_price")?;
    }
    Ok(out)
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ViewState {
    Indeterminate,
    Unknown,
    Working,
    PartiallyFilled,
    Filled,
    Rejected,
    Cancelled,
    Expired,
}
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum Action {
    #[default]
    Quarantine,
    Redrive,
    Adopt,
    Leave,
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum QuarantineReason {
    TerminalOrder,
    AcknowledgementContradiction,
    IndeterminateVenue,
}
pub fn reconcile(st: &OrderState, view: ViewState) -> (Action, Option<QuarantineReason>) {
    if is_terminal(st) {
        return (Action::Quarantine, Some(QuarantineReason::TerminalOrder));
    }
    match view {
        ViewState::Working => (Action::Leave, None),
        ViewState::PartiallyFilled
        | ViewState::Filled
        | ViewState::Rejected
        | ViewState::Cancelled
        | ViewState::Expired => (Action::Adopt, None),
        ViewState::Unknown if st.venue_ack_at.is_none() => (Action::Redrive, None),
        ViewState::Unknown => (
            Action::Quarantine,
            Some(QuarantineReason::AcknowledgementContradiction),
        ),
        ViewState::Indeterminate => (
            Action::Quarantine,
            Some(QuarantineReason::IndeterminateVenue),
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::proto::{
        common::v1::Decimal,
        order::v1::{OrderType, Side, TimeInForce},
    };
    fn d(c: i64) -> Decimal {
        Decimal {
            coefficient: c,
            exponent: 0,
        }
    }
    fn now() -> Timestamp {
        Timestamp {
            seconds: 123,
            nanos: 456,
        }
    }
    fn state() -> OrderState {
        OrderState {
            order_id: "o".into(),
            status: OrderStatus::Routed as i32,
            order_type: OrderType::Limit as i32,
            ordered_quantity: Some(d(10)),
            filled_quantity: Some(d(2)),
            leaves_quantity: Some(d(8)),
            ..Default::default()
        }
    }
    #[test]
    fn future_field_and_nested_unknown_survive_clone_transitions() {
        let mut raw = state().encode_to_vec();
        let mut future = vec![10, 3, b'c', b'm', b'd'];
        future.extend([0xa0, 6, 0x81, 0]);
        kanz_proto_wire::put_message(33, &future, &mut raw);
        raw.extend([0xa0, 6, 0x81, 0]);
        let original = Canonical::<OrderState>::decode(&raw).unwrap();
        for candidate in [
            route(&original, now()),
            reject(&original, now()),
            cancel(&original, now()),
            expire(&original, now()),
        ] {
            let candidate = candidate.unwrap();
            let unchanged = fields(candidate.wire(), |n, _| {
                n != number::<OrderState>("status").unwrap()
                    && n != number::<OrderState>("as_of").unwrap()
            })
            .unwrap();
            let before = fields(original.wire(), |n, _| {
                n != number::<OrderState>("status").unwrap()
                    && n != number::<OrderState>("as_of").unwrap()
            })
            .unwrap();
            assert_eq!(unchanged, before);
        }
    }
    #[test]
    fn accept_carries_quantity_unknown_but_not_command_root_unknown() {
        let cmd = SubmitOrder {
            order_id: "o".into(),
            portfolio_id: "p".into(),
            instrument_id: "XOKX:BTC-USD".into(),
            side: Side::Buy as i32,
            order_type: OrderType::Market as i32,
            time_in_force: TimeInForce::Ioc as i32,
            ..Default::default()
        };
        let mut raw = cmd.encode_to_vec();
        let mut quantity = d(3).encode_to_vec();
        quantity.extend([0xa0, 6, 0x81, 0]);
        kanz_proto_wire::put_message(
            number::<SubmitOrder>("quantity").unwrap(),
            &quantity,
            &mut raw,
        );
        raw.extend([0xa0, 6, 0x82, 0]);
        let accepted = accept(&Canonical::decode(&raw).unwrap(), now()).unwrap();
        assert_eq!(
            accepted.message_wire("ordered_quantity").unwrap(),
            Some(quantity.clone())
        );
        assert_eq!(
            accepted.message_wire("leaves_quantity").unwrap(),
            Some(quantity)
        );
        assert!(!accepted.wire().windows(4).any(|w| w == [0xa0, 6, 0x82, 0]));
        assert_eq!(accepted.value().status, OrderStatus::PendingNew as i32);
    }
    #[test]
    fn amend_retains_legacy_price_policy_and_exact_remaining() {
        let cmd = Canonical::from_value(AmendOrder {
            new_quantity: Some(d(2)),
            new_limit_price: Some(d(-1)),
            ..Default::default()
        })
        .unwrap();
        let out = amend(&Canonical::from_value(state()).unwrap(), &cmd, now()).unwrap();
        assert_eq!(out.value().ordered_quantity, Some(d(2)));
        assert_eq!(
            out.value().leaves_quantity,
            Some(Decimal {
                coefficient: 0,
                exponent: -8
            })
        );
        assert_eq!(out.value().limit_price, Some(d(-1)));
        assert_eq!(out.value().status, OrderStatus::Routed as i32);
    }
    #[test]
    fn same_number_unknown_wire_survives_changed_known_fields() {
        let mut raw = state().encode_to_vec();
        let mut unknown = Vec::new();
        kanz_proto_wire::put_message(
            number::<OrderState>("status").unwrap(),
            b"future",
            &mut unknown,
        );
        kanz_proto_wire::put_varint(
            u64::from(number::<OrderState>("as_of").unwrap()) << 3,
            &mut unknown,
        );
        unknown.push(7);
        kanz_proto_wire::put_varint(
            u64::from(number::<OrderState>("leaves_quantity").unwrap()) << 3,
            &mut unknown,
        );
        unknown.push(9);
        raw.extend(&unknown);
        let original = Canonical::<OrderState>::decode(&raw).unwrap();
        let candidate = cancel(&original, now()).unwrap();
        assert!(candidate.wire().ends_with(&unknown));
        assert_eq!(candidate.value().status, OrderStatus::Cancelled as i32);
        assert_eq!(
            candidate.message_wire("leaves_quantity").unwrap(),
            Some(d(8).encode_to_vec())
        );
    }
    #[test]
    fn acknowledgement_presence_and_terminal_state_are_not_guessed() {
        let mut st = state();
        assert_eq!(reconcile(&st, ViewState::Unknown), (Action::Redrive, None));
        st.venue_ack_at = Some(Timestamp::default());
        assert_eq!(
            reconcile(&st, ViewState::Unknown).1,
            Some(QuarantineReason::AcknowledgementContradiction)
        );
        assert_eq!(reconcile(&st, ViewState::Cancelled), (Action::Adopt, None));
        st.status = OrderStatus::Filled as i32;
        assert_eq!(
            reconcile(&st, ViewState::Working).1,
            Some(QuarantineReason::TerminalOrder)
        );
        assert_eq!(Action::default(), Action::Quarantine);
    }
}
