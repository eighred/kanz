# Staged Tokyo messaging isolation

This bundle is not referenced by an active overlay. It proposes namespace-wide
ingress and egress default-deny for the Tokyo singleton messaging namespace;
promotion requires separate target qualification and coordinated bootstrap labels.
It does not retire the repository's broader messaging or observability exemptions
(#232, #1413).

`policies.yaml` allows NATS4222 from the currently configured Tokyo service apps
and portfolio bootstrap, and Redis6379 only from the gateway. Each cross-namespace
peer combines its namespace and app selector. NATS still authenticates individual
SPIFFE identities and applies canonical subject permissions; labels confer only
network reachability. No6222 cluster route exists in the Tokyo singleton config.
NoPod peer needs8222, which is NATS operations HTTP rather than a Prometheus
exporter. Prometheus receives no broker/data-port exemption.

`bootstrap-labels.yaml` labels both the Job and its Pod template. The existing
recovery runner clones Job metadata plus template spec, so template labels alone
would strand backup. Bootstrap/backup need only NATS4222 and cluster DNS53.
SPIRE credentials arrive through the existing UNIX CSI socket. Redis backup uses
localhost exec; NATS monitoring uses host-side port-forward. S3/KMS/STS transfers
run on the operator host, not these Pods. The existing recovery-drill namespace
remains separate. These policies do not qualify hostNetwork isolation, host-side
AWS access, backup custody or the production restore orchestrator.

The workflow creates a new pinned kind/Cilium/SPIRE cluster with no existing
context input. It loads canonical Tokyo NATS configuration, tenancy and bootstrap,
then proves real mTLS publication with the web-bff identity and explicit broker
authorization refusal for an issued, ungranted identity with the same app label.
Synthetic data alone
is used. Account backup runs under the unchanged bootstrap identity. The fixture
extracts the canonical `nats-restore` server/CLI recipe from the recovery script,
uses a separate default-denied namespace with no service token or source identity,
and restores through loopback into an initially empty broker. Its listener is
additionally restricted to loopback. A bounded regular-file archive transfer and
exact restored configuration/count/record comparison are required; the source
inventory must remain unchanged. This does not grant bootstrap same-account
restore authority, qualify S3/KMS custody, or run the full recovery orchestrator.
Authenticated Redis duplicate claims must survive an actual container restart on
its owned emptyDir. Network tests require three timeout observations for denied
peers, positive baseline controls, an actual Prometheus scrape and healthy final
broker/Redis controls. Connection refusal and command failure never count as
network denial. The final additive policy inventory must equal the staged bundle.

Run offline tests with:

```sh
python3 -B -m unittest discover -s tools/tokyo-messaging-proof -p 'test_*.py'
```

Actual qualification is the mandatory `messaging-isolation` workflow job. It
requires Linux/Docker, the pinned installer dependencies and no production
credentials. It never uploads raw Pod logs, certificates, private keys or Redis
credentials. Evidence records the exact source, policy hash, cases and checked
cluster cleanup; interrupted or incomplete runs cannot pass. A successful fixture
does not establish Tokyo enforcement or authorize policy activation.
