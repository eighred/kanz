//! One-shot local qualification protocol, never an execution/admission service.
use base64::{engine::general_purpose::STANDARD, Engine};
use kanz_order_core::{
    aggregate::{self as a, Error},
    proto::order::v1::{AmendOrder, Fill, OrderState, SubmitOrder},
    Canonical,
};
use serde::Deserialize;
use serde_json::{json, Value};
use std::io::{Read, Write};
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct At {
    seconds: i64,
    nanos: i32,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Input {
    operation: String,
    #[serde(default)]
    state: String,
    #[serde(default)]
    command: String,
    #[serde(default)]
    fill: String,
    at: Option<At>,
    #[serde(default)]
    view: String,
}
fn decode<T: kanz_proto_wire::Schema>(value: &str) -> Result<Canonical<T>, Error> {
    let raw = STANDARD
        .decode(value)
        .map_err(|_| Error::UnsupportedInput)?;
    Ok(Canonical::decode(&raw)?)
}
fn evaluate(input: Input) -> Result<Value, Error> {
    let now = || {
        input
            .at
            .as_ref()
            .map(|t| prost_types::Timestamp {
                seconds: t.seconds,
                nanos: t.nanos,
            })
            .ok_or(Error::UnsupportedInput)
    };
    if input.operation == "Accept" {
        let state = a::accept(&decode::<SubmitOrder>(&input.command)?, now()?)?;
        return Ok(json!({"state":STANDARD.encode(state.wire())}));
    }
    let state = decode::<OrderState>(&input.state)?;
    let result = match input.operation.as_str() {
        "Route" => a::route(&state, now()?)?,
        "Reject" => a::reject(&state, now()?)?,
        "ApplyFill" => a::apply_fill(&state, &decode::<Fill>(&input.fill)?, now()?)?,
        "Amend" => a::amend(&state, &decode::<AmendOrder>(&input.command)?, now()?)?,
        "Cancel" | "Expire" => {
            let result = if input.operation == "Cancel" {
                a::cancel(&state, now()?)?
            } else {
                a::expire(&state, now()?)?
            };
            return Ok(
                json!({"state":STANDARD.encode(result.wire()),"quantity":state.message_wire("leaves_quantity")?.map(|q|STANDARD.encode(q))}),
            );
        }
        "IsTerminal" => return Ok(json!({"terminal":a::is_terminal(state.value())})),
        "Reconcile" => {
            let view = match input.view.as_str() {
                "Indeterminate" | "Future" => a::ViewState::Indeterminate,
                "Unknown" => a::ViewState::Unknown,
                "Working" => a::ViewState::Working,
                "PartiallyFilled" => a::ViewState::PartiallyFilled,
                "Filled" => a::ViewState::Filled,
                "Rejected" => a::ViewState::Rejected,
                "Cancelled" => a::ViewState::Cancelled,
                "Expired" => a::ViewState::Expired,
                _ => return Err(Error::UnsupportedInput),
            };
            let (action, reason) = a::reconcile(state.value(), view);
            let action = match action {
                a::Action::Quarantine => "QUARANTINE",
                a::Action::Redrive => "REDRIVE",
                a::Action::Adopt => "ADOPT",
                a::Action::Leave => "LEAVE",
            };
            let reason = reason.map(|r| match r {
                a::QuarantineReason::TerminalOrder => "TerminalOrder",
                a::QuarantineReason::AcknowledgementContradiction => "AcknowledgementContradiction",
                a::QuarantineReason::IndeterminateVenue => "IndeterminateVenue",
            });
            return Ok(json!({"action":action,"reason":reason}));
        }
        _ => return Err(Error::UnsupportedInput),
    };
    Ok(json!({"state":STANDARD.encode(result.wire())}))
}
fn run() -> Result<(), ()> {
    let mut raw = Vec::new();
    std::io::stdin()
        .take(2 * 1024 * 1024 + 1)
        .read_to_end(&mut raw)
        .map_err(|_| ())?;
    let result = if raw.len() > 2 * 1024 * 1024 {
        Err(Error::Capacity)
    } else {
        serde_json::from_slice::<Input>(&raw)
            .map_err(|_| Error::UnsupportedInput)
            .and_then(evaluate)
    };
    let mut out = match result {
        Ok(mut value) => {
            value["status"] = json!("ok");
            value
        }
        Err(Error::Rejected(code)) => json!({"status":"rejected","code":code}),
        Err(Error::UnsupportedInput) => json!({"status":"unsupported"}),
        Err(Error::NumericalFailure) => json!({"status":"numerical"}),
        Err(Error::Capacity) => json!({"status":"capacity"}),
    };
    out["version"] = json!(env!("KANZ_BUILD_VERSION"));
    let raw = serde_json::to_vec(&out).map_err(|_| ())?;
    std::io::stdout().write_all(&raw).map_err(|_| ())
}
fn main() {
    if run().is_err() {
        eprintln!("aggregate fixture I/O refused");
        std::process::exit(1)
    }
}
