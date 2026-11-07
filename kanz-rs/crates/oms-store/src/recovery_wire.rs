//! Concrete recovered-FACT recipes, not callbacks or authorization capabilities.
//! Shared immutable evidence is expanded only for the current execution.
use crate::{fee_revision, fee_wire::FillWire, recovery_repository, Error, Record, MAX_BYTES};
use kanz_order_core::{
    proto::order::v1::{
        ExecutionFeeApproval, ExecutionFeeCorrectionApproved, ExecutionRecoveryProvenance, Fill,
    },
    Canonical,
};
use kanz_proto_wire::Schema;
use prost::Message;
use std::sync::Arc;

pub struct Context {
    pub(crate) case_id: String,
    pub(crate) mapping: String,
    pub(crate) cursor: String,
    pub(crate) digest: String,
    observed_at: Vec<u8>,
}
impl Context {
    pub fn new(
        case_id: String,
        mapping: String,
        cursor: String,
        digest: String,
        observed_at: Vec<u8>,
    ) -> Result<Arc<Self>, Error> {
        if !recovery_repository::text(&case_id, 256)
            || !recovery_repository::text(&mapping, 256)
            || !recovery_repository::text(&cursor, 1024)
            || digest.len() != 64
            || !digest.bytes().all(|b| b.is_ascii_hexdigit())
            || observed_at.len() > recovery_repository::HISTORY_BYTES
        {
            return Err(Error::InvalidInput);
        }
        Ok(Arc::new(Self {
            case_id,
            mapping,
            cursor,
            digest,
            observed_at,
        }))
    }
}
pub struct FeeTerms {
    source: Arc<Canonical<ExecutionFeeCorrectionApproved>>,
    previous_fee: Vec<u8>,
    previous_digest: String,
}
impl FeeTerms {
    pub fn new(
        source: Arc<Canonical<ExecutionFeeCorrectionApproved>>,
        previous: &Canonical<Fill>,
    ) -> Result<Self, Error> {
        Ok(Self {
            source,
            previous_fee: previous.message_wire("fee")?.ok_or(Error::FeeRevision)?,
            previous_digest: fee_revision::economic_digest(previous.value())?,
        })
    }
    fn materialize(&self, case: &str) -> Result<Vec<u8>, Error> {
        let source = self.source.value();
        if source.case_id != case {
            return Err(Error::FeeRevision);
        }
        let value = ExecutionFeeApproval {
            proposal_id: case.into(),
            digest: source.digest.clone(),
            proposer: source.proposer.clone(),
            approver: source.approver.clone(),
            previous_execution_digest: self.previous_digest.clone(),
            ..Default::default()
        };
        if value.encoded_len() > MAX_BYTES {
            return Err(Error::Capacity);
        }
        let mut raw = value.encode_to_vec();
        let descriptor = ExecutionFeeApproval::descriptor()?;
        append(
            &mut raw,
            descriptor
                .get_field_by_name("approved_at")
                .ok_or(Error::InvalidInput)?
                .number(),
            &self
                .source
                .message_wire("approved_at")?
                .ok_or(Error::FeeRevision)?,
        )?;
        append(
            &mut raw,
            descriptor
                .get_field_by_name("previous_fee")
                .ok_or(Error::InvalidInput)?
                .number(),
            &self.previous_fee,
        )?;
        Ok(raw)
    }
}
pub struct Execution {
    pub(crate) original: FillWire,
    pub(crate) context: Arc<Context>,
    pub(crate) fee: Option<FeeTerms>,
}
impl Execution {
    pub fn new(
        original: FillWire,
        context: Arc<Context>,
        fee: Option<FeeTerms>,
    ) -> Result<Self, Error> {
        if original.value().recovery.is_some() {
            return Err(Error::InvalidInput);
        }
        Ok(Self {
            original,
            context,
            fee,
        })
    }
    pub fn materialize(&self) -> Result<FillWire, Error> {
        let context = &self.context;
        let value = ExecutionRecoveryProvenance {
            case_id: context.case_id.clone(),
            mapping_version: context.mapping.clone(),
            source_cursor: context.cursor.clone(),
            payload_digest: context.digest.clone(),
            execution_digest: recovery_repository::digest(&self.original.journal_bytes()?),
            ..Default::default()
        };
        let mut raw = value.encode_to_vec();
        let descriptor = ExecutionRecoveryProvenance::descriptor()?;
        append(
            &mut raw,
            descriptor
                .get_field_by_name("observed_at")
                .ok_or(Error::InvalidInput)?
                .number(),
            &context.observed_at,
        )?;
        if let Some(fee) = &self.fee {
            append(
                &mut raw,
                descriptor
                    .get_field_by_name("fee_approval")
                    .ok_or(Error::InvalidInput)?
                    .number(),
                &fee.materialize(&context.case_id)?,
            )?;
        }
        let mut fill = self.original.original().to_vec();
        append(
            &mut fill,
            Fill::descriptor()?
                .get_field_by_name("recovery")
                .ok_or(Error::InvalidInput)?
                .number(),
            &raw,
        )?;
        FillWire::decode(&fill)
    }
    pub(crate) fn record(&self, state: &[u8], status: &Record) -> Result<Record, Error> {
        let fill = self.materialize()?;
        let mut payload = Vec::new();
        append(&mut payload, 1, &fill.journal_bytes()?)?;
        append(&mut payload, 2, state)?;
        Ok(Record {
            tenant: status.tenant.clone(),
            partition_key: status.partition_key.clone(),
            subject: "order.order.execution_recovered".into(),
            event_type: "order.order.execution_recovered".into(),
            event_class: 1,
            schema_version: 1,
            domain: "order".into(),
            payload_schema_ref: "order.v1.ExecutionRecovered:1".into(),
            event_time: status.event_time,
            correlation_id: status.correlation_id.clone(),
            causation_id: status.causation_id.clone(),
            trace_context: status.trace_context.clone(),
            payload,
        })
    }
}
fn append(out: &mut Vec<u8>, number: u32, bytes: &[u8]) -> Result<(), Error> {
    kanz_proto_wire::append_message(number, bytes, out, kanz_proto_wire::Budget::Standard)
        .map_err(Error::from)
}

/// Bound retained raw inputs, separately from transient expanded FACTs. These
/// are wire-storage limits, not a measured process RSS guarantee.
pub(crate) fn validate_resident(executions: &[Execution]) -> Result<(), Error> {
    if executions.len() > recovery_repository::MAX_FILLS {
        return Err(Error::Capacity);
    }
    let mut raw = 0usize;
    let mut fees = 0usize;
    let mut source: Option<&Arc<Canonical<ExecutionFeeCorrectionApproved>>> = None;
    let context = executions.first().map(|e| &e.context);
    for execution in executions {
        if context.is_some_and(|c| !Arc::ptr_eq(c, &execution.context)) {
            return Err(Error::InvalidInput);
        }
        raw = raw
            .checked_add(execution.original.original().len())
            .ok_or(Error::Capacity)?;
        if raw > recovery_repository::HISTORY_BYTES {
            return Err(Error::Capacity);
        }
        if let Some(fee) = &execution.fee {
            if source.is_some_and(|s| !Arc::ptr_eq(s, &fee.source)) {
                return Err(Error::InvalidInput);
            }
            source = Some(&fee.source);
            fees = fees
                .checked_add(fee.previous_fee.len())
                .ok_or(Error::Capacity)?;
            if fees > recovery_repository::HISTORY_BYTES
                || fee.source.wire().len() > recovery_repository::HISTORY_BYTES
            {
                return Err(Error::Capacity);
            }
        }
    }
    Ok(())
}
