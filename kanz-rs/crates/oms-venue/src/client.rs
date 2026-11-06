use crate::{
    proto::venue::v1 as venue,
    wire::{self, Schema, WireCodec},
    Canonical,
};
use kanz_order_core::proto::order::v1::{Fill, OrderState};
use std::time::Duration;
use tonic::{transport::Channel, Code, Request, Status};

fn rpc_error(status: Status) -> Status {
    // Tonic 0.14 maps its local transport timeout to Cancelled. Normalize only
    // the typed local cause, never a remote status code or message string.
    let mut cause = std::error::Error::source(&status);
    for _ in 0..16 {
        let Some(error) = cause else { break };
        if error.is::<tonic::TimeoutExpired>() {
            return Status::deadline_exceeded("venue RPC deadline");
        }
        cause = error.source();
    }
    status
}

/// No public arbitrary Channel constructor: only the authenticated connector may build this owner.
pub struct Client {
    pub(crate) channel: Option<Channel>,
    pub(crate) tenant: String,
    pub(crate) owner: crate::transport::Owner,
}
pub use kanz_order_core::aggregate::ViewState;
#[derive(Debug)]
pub struct OrderView {
    pub state: ViewState,
    /// None is the older adapter's Unimplemented answer, never affirmative Unknown.
    pub response: Option<Canonical<venue::QueryOrderResponse>>,
    pub fills: Vec<Canonical<Fill>>,
}
#[derive(Debug, Clone, Copy)]
pub struct Requirements {
    pub verified_account: bool,
    pub declared_order_types: bool,
}
#[derive(Debug)]
pub struct Registration {
    pub identity: Canonical<venue::DescribeResponse>,
    /// A failed discovery is distinct from a successful empty catalog.
    pub instruments: Result<Canonical<venue::ListInstrumentsResponse>, Code>,
}
impl Client {
    async fn call<A: Schema, B: Schema>(
        &self,
        method: &'static str,
        value: Canonical<A>,
        deadline: Duration,
    ) -> Result<Canonical<B>, Status> {
        self.call_budget(method, value, deadline, kanz_proto_wire::Budget::Standard).await
    }
    async fn call_budget<A: Schema, B: Schema>(
        &self, method: &'static str, value: Canonical<A>, deadline: Duration,
        budget: kanz_proto_wire::Budget,
    ) -> Result<Canonical<B>, Status> {
        if budget == kanz_proto_wire::Budget::RecoveryHistory
            && (method != "/venue.v1.VenueAdapterService/QueryOrder" || B::NAME != "venue.v1.QueryOrderResponse") {
            return Err(Status::invalid_argument("recovery response scope"));
        }
        if deadline.is_zero() || deadline > Duration::from_secs(120) {
            return Err(Status::invalid_argument("RPC deadline outside bounds"));
        }
        let channel = self
            .channel
            .as_ref()
            .ok_or_else(|| Status::unavailable("venue client closed"))?
            .clone();
        let mut grpc = tonic::client::Grpc::new(channel)
            .max_decoding_message_size(budget.bytes())
            .max_encoding_message_size(kanz_proto_wire::MAX_BYTES);
        let mut request = Request::new(value);
        request.set_timeout(deadline);
        tokio::time::timeout(deadline, async {
            grpc.ready()
                .await
                .map_err(|_| Status::unavailable("venue transport unavailable"))?;
            grpc.unary(
                request,
                http::uri::PathAndQuery::from_static(method),
                match budget {
                    kanz_proto_wire::Budget::Standard => WireCodec::<A, B>::default(),
                    kanz_proto_wire::Budget::RecoveryHistory => WireCodec::<A, B>::recovery_query(),
                },
            )
            .await
            .map(tonic::Response::into_inner)
            .map_err(rpc_error)
        })
        .await
        .map_err(|_| Status::deadline_exceeded("venue RPC deadline"))?
    }
    pub async fn execute(
        &self,
        state: &Canonical<OrderState>,
        deadline: Duration,
    ) -> Result<Vec<Canonical<Fill>>, Status> {
        let result = self.execute_response(state, deadline).await?;
        wire::messages(&result, "fills")
    }
    pub async fn execute_response(
        &self,
        state: &Canonical<OrderState>,
        deadline: Duration,
    ) -> Result<Canonical<venue::ExecuteResponse>, Status> {
        let request = wire::with_state(
            venue::ExecuteRequest {
                tenant_id: self.tenant.clone(),
                state: None,
            },
            state,
        )?;
        self.call("/venue.v1.VenueAdapterService/Execute", request, deadline)
            .await
    }
    /// Only OK confirms withdrawal. Deadline/transport failure remains an error.
    pub async fn cancel_order(
        &self,
        state: &Canonical<OrderState>,
        deadline: Duration,
    ) -> Result<(), Status> {
        let request = wire::with_state(
            venue::CancelOrderRequest {
                tenant_id: self.tenant.clone(),
                state: None,
            },
            state,
        )?;
        let _: Canonical<venue::CancelOrderResponse> = self
            .call(
                "/venue.v1.VenueAdapterService/CancelOrder",
                request,
                deadline,
            )
            .await?;
        Ok(())
    }
    pub async fn describe(
        &self,
        deadline: Duration,
    ) -> Result<Canonical<venue::DescribeResponse>, Status> {
        self.call(
            "/venue.v1.VenueAdapterService/Describe",
            Canonical::from_value(venue::DescribeRequest {})?,
            deadline,
        )
        .await
    }
    pub async fn list_instruments(
        &self,
        deadline: Duration,
    ) -> Result<Canonical<venue::ListInstrumentsResponse>, Status> {
        self.call(
            "/venue.v1.VenueAdapterService/ListInstruments",
            Canonical::from_value(venue::ListInstrumentsRequest {})?,
            deadline,
        )
        .await
    }
    pub async fn query_order(
        &self,
        state: &Canonical<OrderState>,
        deadline: Duration,
    ) -> Result<OrderView, Status> {
        self.query_order_budget(state, deadline, kanz_proto_wire::Budget::Standard).await
    }
    /// Existing complete-history contract: only this QueryOrder response may reach4MiB.
    pub async fn query_recovery_history(&self, state: &Canonical<OrderState>, deadline: Duration) -> Result<OrderView, Status> {
        self.query_order_budget(state, deadline, kanz_proto_wire::Budget::RecoveryHistory).await
    }
    async fn query_order_budget(&self, state: &Canonical<OrderState>, deadline: Duration, budget: kanz_proto_wire::Budget) -> Result<OrderView, Status> {
        let request = wire::with_state(
            venue::QueryOrderRequest {
                tenant_id: self.tenant.clone(),
                state: None,
            },
            state,
        )?;
        let response: Canonical<venue::QueryOrderResponse> = match self
            .call_budget(
                "/venue.v1.VenueAdapterService/QueryOrder",
                request,
                deadline,
                budget,
            )
            .await
        {
            Ok(value) => value,
            Err(error) if error.code() == Code::Unimplemented => {
                return Ok(OrderView {
                    state: ViewState::Indeterminate,
                    response: None,
                    fills: vec![],
                })
            }
            Err(error) => return Err(error),
        };
        let state = match venue::OrderViewState::try_from(response.value().state) {
            Ok(venue::OrderViewState::Unknown) => ViewState::Unknown,
            Ok(venue::OrderViewState::Working) => ViewState::Working,
            Ok(venue::OrderViewState::PartiallyFilled) => ViewState::PartiallyFilled,
            Ok(venue::OrderViewState::Filled) => ViewState::Filled,
            Ok(venue::OrderViewState::Rejected) => ViewState::Rejected,
            Ok(venue::OrderViewState::Cancelled) => ViewState::Cancelled,
            Ok(venue::OrderViewState::Expired) => ViewState::Expired,
            _ => ViewState::Indeterminate,
        };
        let fills = wire::messages(&response, "fills")?;
        Ok(OrderView {
            state,
            response: Some(response),
            fills,
        })
    }
    /// Startup refusal closes the connection, matching configuredVenues' fatal path.
    pub async fn register(
        &mut self,
        mic: &str,
        account: &str,
        requirements: Requirements,
        deadline: Duration,
    ) -> Result<Registration, Status> {
        let result = self
            .register_inner(mic, account, requirements, deadline)
            .await;
        if result.is_err() {
            self.channel.take();
            self.owner.close().await;
        }
        result
    }
    async fn register_inner(
        &self,
        mic: &str,
        account: &str,
        requirements: Requirements,
        deadline: Duration,
    ) -> Result<Registration, Status> {
        let identity = self.describe(deadline).await?;
        let id = identity.value();
        if id.mic != mic || id.account != account {
            return Err(Status::failed_precondition("venue identity mismatch"));
        }
        if requirements.verified_account && !id.account_verified {
            return Err(Status::failed_precondition("venue account unverified"));
        }
        if requirements.declared_order_types && id.supported_order_types.is_empty() {
            return Err(Status::failed_precondition("venue order types undeclared"));
        }
        let instruments = self.list_instruments(deadline).await.map_err(|e| e.code());
        Ok(Registration {
            identity,
            instruments,
        })
    }
}

#[cfg(test)]
mod deadline_tests {
    use super::*;
    #[test]
    fn only_typed_local_timeout_is_deadline_exceeded() {
        let local = Status::from_error(Box::new(tonic::TimeoutExpired(())));
        assert_eq!(local.code(), Code::Cancelled);
        assert_eq!(rpc_error(local).code(), Code::DeadlineExceeded);
        let remote = Status::cancelled(tonic::TimeoutExpired(()).to_string());
        assert_eq!(rpc_error(remote).code(), Code::Cancelled);
        assert_eq!(
            rpc_error(Status::unavailable("fixture")).code(),
            Code::Unavailable
        );
    }
}
