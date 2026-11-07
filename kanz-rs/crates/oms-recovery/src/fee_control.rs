//! Existing Go dualcontrol semantics for the fee-correction act only. This
//! validates a delegated human decision; it neither authenticates an actor nor
//! grants permission to change economics outside the immutable proposal.
use crate::Error;
use kanz_oms_store::fee_subject;
use kanz_order_core::{proto::order::v1::ExecutionFeeCorrectionProposed, Canonical};
use kanz_proto_wire::Schema;
use sha2::{Digest, Sha256};

pub(crate) const DEFAULT_TTL_SECONDS: i64 = 24 * 60 * 60;

#[derive(Debug, PartialEq, Eq)]
pub(crate) enum Refusal {
    Malformed,
    SelfApproval,
    PayloadChanged,
    Expired,
}

pub(crate) fn stamp(t: Option<&prost_types::Timestamp>) -> Result<i128, Error> {
    let t = t.ok_or(Error::FeeRevision)?;
    if !(-62135596800..=253402300799).contains(&t.seconds) || !(0..1_000_000_000).contains(&t.nanos)
    {
        return Err(Error::FeeRevision);
    }
    Ok(i128::from(t.seconds) * 1_000_000_000 + i128::from(t.nanos))
}

pub(crate) fn digest(
    tenant: &str,
    proposal: &Canonical<ExecutionFeeCorrectionProposed>,
) -> Result<String, Error> {
    if tenant.is_empty() {
        return Err(Error::FeeRevision);
    }
    let number = ExecutionFeeCorrectionProposed::descriptor()?
        .get_field_by_name("digest")
        .ok_or(Error::FeeRevision)?
        .number();
    let mut pos = 0;
    let mut raw = Vec::new();
    while pos < proposal.wire().len() {
        let start = pos;
        let (n, w) = kanz_proto_wire::key(proposal.wire(), &mut pos)?;
        kanz_proto_wire::skip(proposal.wire(), &mut pos, n, w)?;
        // An unknown wrong-wire occurrence at this number remains in the hash.
        if !(n == number && w == 2) {
            raw.extend_from_slice(&proposal.wire()[start..pos]);
        }
    }
    let mut h = Sha256::new();
    for part in [
        b"EXECUTION_FEE_CORRECTION".as_slice(),
        tenant.as_bytes(),
        &raw,
    ] {
        h.update((part.len() as u64).to_be_bytes());
        h.update(part);
    }
    Ok(format!("{:x}", h.finalize()))
}

pub(crate) struct Control<'a> {
    proposer: &'a str,
    digest: &'a str,
    pub created: i128,
    expires: i128,
}
impl<'a> Control<'a> {
    pub fn new(p: &'a ExecutionFeeCorrectionProposed) -> Result<Self, Error> {
        let created = stamp(p.proposed_at.as_ref())?;
        let expiry = stamp(p.expires_at.as_ref())?;
        // time.Sub saturates before Go Propose adds the duration back.
        let ttl = (expiry - created).min(i128::from(i64::MAX));
        if fee_subject::trim(&p.case_id).is_empty()
            || fee_subject::trim(&p.order_id).is_empty()
            || fee_subject::normalize(&p.proposer).is_empty()
            || fee_subject::trim(&p.digest).is_empty()
            || created == -62135596800i128 * 1_000_000_000
            || ttl <= 0
        {
            return Err(Error::ApprovalRefused);
        }
        Ok(Self {
            proposer: &p.proposer,
            digest: &p.digest,
            created,
            expires: created + ttl,
        })
    }
    pub fn approve(&self, actor: &str, digest: &str, now: i128) -> Result<(), Refusal> {
        let actor = fee_subject::normalize(actor);
        if fee_subject::normalize(self.proposer).is_empty()
            || self.expires == -62135596800i128 * 1_000_000_000
            || actor.is_empty()
        {
            return Err(Refusal::Malformed);
        }
        // Keep the Go refusal precedence: self, digest, then expiry. The command
        // handler persists distinct blocked reasons for these three outcomes.
        if actor == fee_subject::normalize(self.proposer) {
            return Err(Refusal::SelfApproval);
        }
        if digest != self.digest {
            return Err(Refusal::PayloadChanged);
        }
        if now >= self.expires {
            return Err(Refusal::Expired);
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use prost::Message;
    #[test]
    fn digest_retains_unknown_wire_at_digest_number() {
        let p = ExecutionFeeCorrectionProposed {
            case_id: "case".into(),
            digest: "first".into(),
            ..Default::default()
        };
        let mut raw = p.encode_to_vec();
        // Known digest is field 6/bytes; field 6/varint remains unknown.
        raw.extend_from_slice(&[48, 17]);
        let first = Canonical::decode(&raw).unwrap();
        let mut second_raw = ExecutionFeeCorrectionProposed {
            digest: "second".into(),
            ..p.clone()
        }
        .encode_to_vec();
        second_raw.extend_from_slice(&[48, 17]);
        let second = Canonical::decode(&second_raw).unwrap();
        assert_eq!(
            digest("tenant", &first).unwrap(),
            digest("tenant", &second).unwrap()
        );
        *second_raw.last_mut().unwrap() = 18;
        let altered = Canonical::decode(&second_raw).unwrap();
        assert_ne!(
            digest("tenant", &first).unwrap(),
            digest("tenant", &altered).unwrap()
        );
        assert_ne!(
            digest("tenant", &first).unwrap(),
            digest("other", &first).unwrap()
        );
        assert_eq!(digest("", &first), Err(Error::FeeRevision));
    }
    #[test]
    fn refusal_order_and_committed_signing_time_are_preserved() {
        let p = ExecutionFeeCorrectionProposed {
            case_id: "case".into(),
            order_id: "order".into(),
            proposer: "user:ALICE".into(),
            digest: "digest".into(),
            proposed_at: Some(prost_types::Timestamp {
                seconds: 100,
                nanos: 0,
            }),
            expires_at: Some(prost_types::Timestamp {
                seconds: 110,
                nanos: 0,
            }),
            ..Default::default()
        };
        let c = Control::new(&p).unwrap();
        assert_eq!(
            c.approve(" user:alice ", "wrong", 111_000_000_000),
            Err(Refusal::SelfApproval)
        );
        assert_eq!(
            c.approve("user:bob", "wrong", 111_000_000_000),
            Err(Refusal::PayloadChanged)
        );
        assert_eq!(
            c.approve("user:bob", "digest", 110_000_000_000),
            Err(Refusal::Expired)
        );
        assert_eq!(c.approve("user:bob", "digest", 109_999_999_999), Ok(()));
        // Stored approvals are checked at their original time, not retry time.
        assert_eq!(c.approve("user:bob", "digest", 105_000_000_000), Ok(()));
    }
}
