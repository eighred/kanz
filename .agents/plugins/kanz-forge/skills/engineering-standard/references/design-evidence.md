# Financial design evidence

Use external sources to answer a specific design question. Do not include a
whole upstream repository, registry, or financial glossary in the prompt.

1. **Required outcome.** Name the decision, actor, timing, instrument and failure
   consequence. Cite an approved Kanz requirement, mandate, issue or attributable
   user specification. A competitor feature is a comparison, not a Kanz requirement.
2. **Current path.** Follow the served interface to its data owner and consumers.
   Classify the gap: missing model, unavailable terms, unwired implementation,
   missing entitlement, or missing evidence. Record the revision. Finding a
   formula in a library does not establish a usable product capability.
3. **Method choice.** Prefer a relevant primary methodological source or
   licensed owner-supplied specification. Record source, date/version, convention,
   permitted use, assumptions and alternatives. Use an independently checked
   implementation as an oracle. A reference repo is neither policy nor training data.
4. **Implementation decision.** Specify typed inputs, result units, point-in-time
   semantics, failure behavior, owning package and the smallest discriminating
   example. If the required definition is unknown, ask for that definition;
   do not fill the gap with an alleged proprietary algorithm.

Example: BlackRock publicly describes portfolio stress testing and scenario
analysis ([Aladdin Risk](https://www.blackrock.com/aladdin/platforms/products/aladdin-risk)).
That supports a capability comparison. It does not specify how to calibrate
a Kanz shock or establish BlackRock's private pricing implementation. For a
request to reproduce an observed stress episode, inspect Kanz scenario code and
issue #1232 first: a manually specified shock and an immutable observed market
history are different inputs. Decide which is required before implementing math.

The Anthropic financial-services repository is an optional workflow reference
only. Do not use it as evidence of a pricing method, Aladdin internals or Kanz
requirements. Prefer an approved in-house source where available; keep licensed
data outside model context unless the owner authorizes that use.
