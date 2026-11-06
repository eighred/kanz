use kanz_decimal_compat::{compare, in_domain};
use kanz_order_core::proto::order::v1::Fill;
use sha2::{Digest, Sha256};
use std::{cmp::Ordering, fmt::Write};

/// Exact Go encoding/json string escaping used by fillfact.ExecutionKey.
fn quoted(s: &str, out: &mut String) {
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\x08' => out.push_str("\\b"),
            '\x0c' => out.push_str("\\f"),
            '<' | '>' | '&' | '\u{2028}' | '\u{2029}' | '\0'..='\x1f' => {
                write!(out, "\\u{:04x}", c as u32).expect("writing to String");
            }
            _ => out.push(c),
        }
    }
    out.push('"');
}
pub fn execution_key(f: &Fill) -> String {
    let fields = [
        &f.venue,
        &f.venue_account_id,
        &f.instrument_id,
        &f.venue_execution_id,
    ];
    if fields.iter().any(|s| s.is_empty()) {
        return f.fill_id.clone();
    }
    let mut encoded = String::from("[");
    for (i, s) in fields.iter().enumerate() {
        if i > 0 {
            encoded.push(',');
        }
        quoted(s, &mut encoded);
    }
    encoded.push(']');
    format!("execution:v1:{:x}", Sha256::digest(encoded.as_bytes()))
}
pub(crate) fn domain(f: &Fill) -> bool {
    [
        f.quantity.as_ref(),
        f.price.as_ref(),
        f.fee.as_ref().and_then(|v| v.amount.as_ref()),
        f.recovery
            .as_ref()
            .and_then(|r| r.fee_approval.as_ref())
            .and_then(|a| a.previous_fee.as_ref())
            .and_then(|v| v.amount.as_ref()),
    ]
    .into_iter()
    .all(in_domain)
}
pub(crate) fn timestamp(t: &prost_types::Timestamp) -> bool {
    (-62135596800..=253402300799).contains(&t.seconds) && (0..1_000_000_000).contains(&t.nanos)
}
pub fn same_execution(a: &Fill, b: &Fill) -> bool {
    let aa = a.fee.as_ref().and_then(|f| f.amount.as_ref());
    let ba = b.fee.as_ref().and_then(|f| f.amount.as_ref());
    domain(a)
        && domain(b)
        && a.order_id == b.order_id
        && a.venue == b.venue
        && a.venue_account_id == b.venue_account_id
        && a.instrument_id == b.instrument_id
        && a.venue_execution_id == b.venue_execution_id
        && a.side == b.side
        && a.fee.as_ref().map_or("", |f| f.currency_code.as_str())
            == b.fee.as_ref().map_or("", |f| f.currency_code.as_str())
        && a.quantity.is_some() == b.quantity.is_some()
        && a.price.is_some() == b.price.is_some()
        && aa.is_some() == ba.is_some()
        && a.executed_at == b.executed_at
        && a.executed_at.as_ref().is_none_or(timestamp)
        && compare(a.quantity.as_ref(), b.quantity.as_ref()) == Ordering::Equal
        && compare(a.price.as_ref(), b.price.as_ref()) == Ordering::Equal
        && compare(aa, ba) == Ordering::Equal
}
pub(crate) fn same_except_fee(a: &Fill, b: &Fill) -> bool {
    let (Some(af), Some(bf)) = (&a.fee, &b.fee) else {
        return false;
    };
    if af.amount.is_none()
        || bf.amount.is_none()
        || af.currency_code.is_empty()
        || af.currency_code != bf.currency_code
    {
        return false;
    }
    let (mut a, mut b) = (a.clone(), b.clone());
    a.fee = None;
    b.fee = None;
    same_execution(&a, &b)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn go_string_encoding() {
        let mut s = String::new();
        quoted("<>&\u{2028}\u{2029}\n\"\\東京", &mut s);
        assert_eq!(s, "\"\\u003c\\u003e\\u0026\\u2028\\u2029\\n\\\"\\\\東京\"");
    }
    #[test]
    fn legacy_identity_is_not_renamed() {
        let f = Fill {
            fill_id: "legacy".into(),
            venue: "venue".into(),
            ..Default::default()
        };
        assert_eq!(execution_key(&f), "legacy");
    }
}
