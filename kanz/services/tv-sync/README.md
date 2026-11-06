# tv-sync listener migration

This source release supports an opt-in split while keeping existing deployments
compatible. With `TV_SYNC_API_LISTEN` absent, `TV_SYNC_LISTEN` (default `:8091`)
continues to serve the Broker API, health, readiness and metrics together. This
legacy mode retains the monitoring-to-tenant-API exposure tracked in #232; it is
not an isolation claim. The deployment manifests select split mode and the guard
entry belongs in the split-listener map. Application and migration images must
come from the same verified release before those manifests are applied.

Set `TV_SYNC_API_LISTEN=:8113` to select the split. The Broker API then exists
only on that listener, while `TV_SYNC_LISTEN` serves only `/healthz`, `/readyz`
and `/metrics`. Explicit empty or malformed API configuration refuses startup;
a failed bind never falls back to the legacy listener. All selected sockets
must bind before readiness and all drain on shutdown.

Broker handlers trust the gateway's `X-Kanz-Principal-*` headers. NetworkPolicy
must make the gateway the API listener's only reachable caller; neither the
headers nor the new listener independently authenticate a gateway workload.

## Release and promotion order

1. Merge and verify the source with the existing deployment manifests intact.
   The source merge leaves the running image and routing intact. The new binary
   retains legacy listener compatibility, but deploying it also requires the
   current migration image and schema.
2. Publish the reviewed merged revision through the approved signed-image path.
   Verify immutable tv-sync and kanz-migrate digests from that same release,
   signer identity, SBOM and vulnerability evidence for each. The migration image
   must include the current tv-sync checkpoint migration as well as the fact log.
   A passing PR build or unsigned main image is not promotion evidence.
3. On a controlled enforcing-CNI test cluster, validate the matching image with
   API 8113 and operations 8091, the two Service ports, gateway URL, and base and
   environment-specific NetworkPolicies. Keep the singleton `Recreate` strategy.
   Verify the target's separate migration and runtime roles, including runtime
   access to the new checkpoint table and the migration owner's default grants.
   A disposable owner-role fixture does not establish those deployed grants.
4. Promote both verified app and init-container digests and listener configuration
   in a reviewed manifest change. Move tv-sync's guard entry from the shared-listener
   map to the split-listener map in that same change. Do not change the gateway
   port while retaining an image that lacks the split.

Before promotion, prove authorized gateway reads succeed on 8113, monitoring
can scrape and probe 8091, Broker routes return 404 on 8091 even with forged
principal headers, and monitoring/unrelated pods cannot connect to 8113 by
Service or pod IP. Check missing-identity refusal, tenant scoping, restart and
fact-log recovery without introducing a second consumer replica.

Retain the matching prior image and manifests for coordinated rollback. Removing
`TV_SYNC_API_LISTEN` restores legacy shared routing, including its exposure;
rolling only the image or only the gateway port can cause an outage. No release
or rollback changes the capital halt.

Loopback HTTP and static policy tests establish source behavior. They do not
prove an image was published or a CNI enforced policy. #232 stays open until
its operational acceptance is observed on the promoted revision.
