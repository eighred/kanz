# MCP governed state reads

The default service offers `get_risk_measures`. Selecting
`MCP_STATE_READS_ENABLED=true` additionally offers `get_positions` and
`get_order_state`. Selection requires `MCP_TENANT`, `MCP_RISK_QUERY_ADDR`,
`MCP_ORDER_STATE_QUERY_ADDR`, and `SPIFFE_ENDPOINT_SOCKET`; partial configuration
refuses startup. Both domain backends must be configured before either new tool
is advertised. This source change does not activate a deployment.

The selected mode binds calls to the configured tenant before an ownership
lookup. It uses the existing portfolio read-scope helper and authorization gate;
denied attempts still reach the canonical decision recorder. Each data response
independently binds its owner and portfolio to the authenticated caller. The
existing gateway-only MCP ingress and authenticated principal projection remain
required. No bearer, cookie, or caller-selected tenant is forwarded to a domain
service.

Every selected domain RPC has a fixed three-second client budget, including
ownership and scalar-measure reads in this mode. An earlier caller deadline
clips that budget, and caller cancellation propagates to the domain. Agent
arguments cannot extend it. Dependency errors retain the existing sanitized
refusal and authorization-decision recording.

`get_positions` uses `risk.read`. It reports **observed risk state**, not an
accounting book or proof that all holdings have arrived. Each row carries its
state time and an exact signed quantity. Missing/malformed quantities, missing
times, stale/degraded responses and unknown trust signals withhold numerical
values as `UNKNOWN`. Coefficients are JSON strings, preserving integers beyond
JavaScript's exact range. An empty observed set is not a claim of a flat book.
The complete observed snapshot is bounded at 1,024 rows. Actual stored cardinality,
including retained flat rows, is checked under the owning aggregate lock before
cloning or sorting. Pages contain at most 100 rows (default 50). A content-bound
cursor refuses a changed snapshot; restart
pagination rather than combine versions. Oversized snapshots refuse without
serving a prefix. The content identity also binds quality flags, so aging from
fresh to stale invalidates continuation without binding unrelated wall-clock
ticks. Historical `as_of` arguments are unsupported and refused.

`get_order_state` requires `portfolio_id`, `order_id`, and `order.read`. No role
grant is added by this change. It reports one **recorded OMS state**, including
its effective time, read time and store version. It is not live venue truth and
offers no history, approval, cancel, amend or execution operation. Missing and
out-of-portfolio orders return the same refusal. Quarantine and unrecognized
lifecycle states withhold usable status and quantities. Account, venue, fee,
actor and investigation details do not enter the projection. A portfolio that
the risk ownership resolver has not observed remains conservatively unavailable,
even if an OMS row exists.

The optional OMS endpoint is selected with `OMS_STATE_QUERY_LISTEN`. It requires
a separate listener, durable database, active spine, owning tenant and SPIFFE
identity. It registers only `query.v1.OrderStateReadService` and admits the
declared `spiffe://kanz.internal/ns/kanz-services/sa/mcp` identity. It never exposes
the neighboring history, pending-approval or instrument-catalogue services.
The MCP client authenticates the declared risk-engine and OMS SVIDs explicitly.
Other identity/topology selections require their own reviewed configuration;
there is no automatic mesh-wide or plaintext fallback for this mode.

The OMS read server imposes a five-second deadline, including pool acquisition
and network work. It admits at most one read at a time against the existing
four-connection execution store; additional reads fail immediately. OMS lifecycle
cancellation cancels accepted reads. Listener shutdown has one five-second budget
for transport, Serve and accepted-Load joins. A noncooperative reader produces an
explicit cleanup refusal: the OMS retains that shared store until fatal process
exit rather than blocking in pool close or claiming joined cleanup. The cleanup
error preserves the original startup/runtime failure classification.

Qualification includes the actual risk Engine and generated protobuf transport,
production MCP authorization/projection, a synthetic SPIFFE mTLS OMS listener,
and restricted-role PostgreSQL read isolation. Run affected packages serially
on Linux, with the real database prerequisite and the architecture import/write
mutation controls. These component checks do not establish gateway identity
configuration, deployed NetworkPolicy, production data completeness or operational
acceptance. Deployment/network selection and complete-chain verification remain
separate from constructing the source capability.
