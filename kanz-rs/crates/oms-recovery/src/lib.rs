#![forbid(unsafe_code)]
//! Observation/recovery coordination. No order submission or venue side-effect RPC.
mod approval;
mod coordinator;
pub use coordinator::{Config, Coordinator, Method};
mod handlers;
mod history;
mod observation;
mod preparation;

#[derive(Debug, PartialEq, Eq)]
pub enum Error {
    InvalidInput,
    Capacity,
    Conflict,
    EvidenceConflict,
    RecoveryEvidenceConflict,
    ApprovalRefused,
    HistoryIncomplete,
    PostedHistoryUnknown,
    FeeRevision,
    Database,
    CommitUnconfirmed,
    PublicationUnconfirmed,
    Venue,
    UnsupportedAction,
    Deadline,
}
impl From<kanz_proto_wire::Error> for Error {
    fn from(e: kanz_proto_wire::Error) -> Self {
        match e {
            kanz_proto_wire::Error::Capacity => Self::Capacity,
            _ => Self::InvalidInput,
        }
    }
}
impl From<kanz_oms_store::Error> for Error {
    fn from(e: kanz_oms_store::Error) -> Self {
        use kanz_oms_store::Error as E;
        match e {
            E::Capacity => Self::Capacity,
            E::Conflict => Self::Conflict,
            E::IdentityConflict => Self::EvidenceConflict,
            E::FeeRevision => Self::FeeRevision,
            E::PostedExecutionUnknown => Self::PostedHistoryUnknown,
            E::CommitUnconfirmed => Self::CommitUnconfirmed,
            E::PublicationUnconfirmed => Self::PublicationUnconfirmed,
            E::InvalidInput => Self::InvalidInput,
            _ => Self::Database,
        }
    }
}
