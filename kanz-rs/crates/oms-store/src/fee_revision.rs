//! Exact fee revision evidence validation. Authentication and dual-control claiming
//! remain the caller's responsibility; these terms grant no independent authority.
use super::{fee_subject::normalize, fee_wire::FillWire, identity, Error};
use kanz_order_core::proto::{
    common::v1::Decimal,
    order::v1::{Fill, Side},
};
use num_bigint::BigInt;
use num_rational::BigRational;
use num_traits::Signed;
use sha2::{Digest, Sha256};

fn rational(d: &Decimal) -> Result<BigRational, Error> {
    if !(-64..=64).contains(&d.exponent) {
        return Err(Error::FeeRevision);
    }
    let power = BigInt::from(10).pow(d.exponent.unsigned_abs());
    Ok(if d.exponent >= 0 {
        BigRational::from_integer(BigInt::from(d.coefficient) * power)
    } else {
        BigRational::new(BigInt::from(d.coefficient), power)
    })
}
fn amount(f: &Fill) -> Result<BigRational, Error> {
    rational(
        f.fee
            .as_ref()
            .and_then(|v| v.amount.as_ref())
            .ok_or(Error::FeeRevision)?,
    )
}
fn utc(t: &prost_types::Timestamp) -> Result<String, Error> {
    if !identity::timestamp(t) || t.seconds <= 0 {
        return Err(Error::FeeRevision);
    }
    let mut days = t.seconds / 86400;
    let mut year = 1970i64;
    let leap = |y: i64| y % 4 == 0 && (y % 100 != 0 || y % 400 == 0);
    loop {
        let n = if leap(year) { 366 } else { 365 };
        if days < n {
            break;
        }
        days -= n;
        year += 1;
    }
    let months = [
        31,
        if leap(year) { 29 } else { 28 },
        31,
        30,
        31,
        30,
        31,
        31,
        30,
        31,
        30,
        31,
    ];
    let mut month = 0;
    while days >= months[month] {
        days -= months[month];
        month += 1;
    }
    let seconds = t.seconds % 86400;
    let fraction = if t.nanos == 0 {
        String::new()
    } else {
        format!(".{:09}", t.nanos).trim_end_matches('0').to_owned()
    };
    Ok(format!(
        "{year:04}-{:02}-{:02}T{:02}:{:02}:{:02}{fraction}Z",
        month + 1,
        days + 1,
        seconds / 3600,
        seconds / 60 % 60,
        seconds % 60
    ))
}
pub fn economic_digest(f: &Fill) -> Result<String, Error> {
    if !identity::domain(f) || f.order_id.is_empty() || identity::execution_key(f) == f.fill_id {
        return Err(Error::IdentityConflict);
    }
    let result = (|| {
        let q = rational(f.quantity.as_ref().ok_or(Error::FeeRevision)?)?;
        let p = rational(f.price.as_ref().ok_or(Error::FeeRevision)?)?;
        let fee = amount(f)?;
        let currency = &f.fee.as_ref().ok_or(Error::FeeRevision)?.currency_code;
        if !q.is_positive() || !p.is_positive() || currency.is_empty() {
            return Err(Error::FeeRevision);
        }
        let side = match Side::try_from(f.side) {
            Ok(v @ (Side::Buy | Side::Sell)) => v.as_str_name(),
            _ => return Err(Error::FeeRevision),
        };
        let time = utc(f.executed_at.as_ref().ok_or(Error::FeeRevision)?)?;
        let parts = [
            "execution-economics-v1".to_owned(),
            f.order_id.clone(),
            identity::execution_key(f),
            side.to_owned(),
            q.to_string(),
            p.to_string(),
            fee.to_string(),
            currency.clone(),
            time,
        ];
        let mut hash = Sha256::new();
        for part in parts {
            hash.update((part.len() as u64).to_be_bytes());
            hash.update(part.as_bytes());
        }
        Ok(format!("{:x}", hash.finalize()))
    })();
    result.map_err(|_| Error::IdentityConflict)
}
pub fn terms(revised: &FillWire) -> Result<FillWire, Error> {
    let f = revised.value();
    let recovery = f.recovery.as_ref().ok_or(Error::FeeRevision)?;
    let a = recovery.fee_approval.as_ref().ok_or(Error::FeeRevision)?;
    if a.proposal_id.is_empty()
        || a.proposal_id != recovery.case_id
        || normalize(&a.proposer).is_empty()
        || normalize(&a.approver).is_empty()
        || normalize(&a.proposer) == normalize(&a.approver)
        || a.proposer.len() > 256
        || a.approver.len() > 256
        || a.proposal_id.len() > 256
        || !identity::domain(f)
    {
        return Err(Error::FeeRevision);
    }
    utc(a.approved_at.as_ref().ok_or(Error::FeeRevision)?)?;
    for digest in [&a.digest, &a.previous_execution_digest] {
        if digest.len() != 64 || !digest.bytes().all(|b| b.is_ascii_hexdigit()) {
            return Err(Error::FeeRevision);
        }
    }
    let previous = revised.previous_fee()?;
    if !identity::same_except_fee(previous.value(), f)
        || amount(previous.value())? == amount(f)?
        || economic_digest(previous.value()).map_err(|_| Error::FeeRevision)?
            != a.previous_execution_digest
    {
        return Err(Error::FeeRevision);
    }
    Ok(previous)
}
pub fn approved_delta(previous: &FillWire, revised: &FillWire) -> Result<BigRational, Error> {
    let approved = terms(revised)?;
    if !identity::same_execution(previous.value(), approved.value()) {
        return Err(Error::FeeRevision);
    }
    Ok(amount(previous.value())? - amount(revised.value())?)
}
