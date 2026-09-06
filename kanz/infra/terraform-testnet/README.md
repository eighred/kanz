# Issue #71 — Tokyo single-node testnet foundation

This is a deliberately isolated, cost-bounded certification environment. It is
not the production EKS topology in `../terraform`, is not highly available, and
does not satisfy the platform's regional DR objective. Its only purpose is to
prove the governed capital path against Binance Testnet and OKX Demo after a
separate restore drill produces a current DR attestation.

## Reviewed envelope

- Tokyo (`ap-northeast-1`), one `t3a.large` in standard CPU-credit mode.
- 20 GiB encrypted gp3 root + 80 GiB encrypted gp3 durable data.
- One stable Elastic IP for outbound exchange allowlisting.
- No inbound security-group rules and no SSH key. Administration is SSM only.
- k3s `v1.36.4+k3s1`, pinned by version and a SHA-256-verified installer at an
  immutable commit; Traefik and ServiceLB are disabled.
- Embedded etcd with encrypted Kubernetes secrets and local snapshots every six
  hours; DLM snapshots the data EBS volume daily and retains seven.
- A mandatory USD 100 monthly budget alerts at USD 50, 75, 90, and 100.

Expected steady-state base cost is approximately USD 86.72/month using the
2026-09-06 Tokyo public prices and 20 GiB average incremental snapshot storage.
Data transfer, excess CPU credits, and snapshot churn are variable. The pinned
standard credit mode prevents T3 Unlimited surplus-credit charges.

## Apply

Use only the MFA-backed `kanz-platform` profile. Never put a credential, token,
exchange key, passphrase, or budget email in a tfvars file.

```powershell
$env:TF_VAR_budget_email = '<billing-alert-address>'
./Invoke-Terraform.ps1 init
./Invoke-Terraform.ps1 plan -out=kanz-testnet.tfplan
./Invoke-Terraform.ps1 show kanz-testnet.tfplan
./Invoke-Terraform.ps1 apply kanz-testnet.tfplan
Remove-Item Env:TF_VAR_budget_email
```

The wrapper bridges AWS CLI v2 `login_session` authentication into Terraform as
an in-memory, short-lived role session and clears it on exit. It never prints or
persists the session credentials.

Terraform state is ignored by git but remains sensitive operational state. This
single-operator testnet starts with local state; migrate it to an encrypted,
versioned S3 backend before adding another operator.

## Bootstrap and capital controls

The instance bootstraps only k3s. Workload images, Vault configuration, SPIRE
registration, and exchange credentials are bound afterward through the existing
GitOps/Vault boundaries. User data contains no secrets. Production exchange
hosts must remain impossible in the testnet overlay.

Do not run `infra/dr/run-capitalpath-certification.sh` until all of these are
true:

1. SSM reports the node online and `/var/lib/rancher/.kanz-bootstrap-complete`
   exists.
2. The testnet workload set is Ready and its images are digest-pinned.
3. Binance Testnet and OKX Demo credentials are mounted from Vault CSI files;
   no key is present in a Kubernetes Secret, environment variable, command
   line, Terraform state, or log.
4. A restore into a replacement node has met RPO/RTO and produced the fresh,
   hashed DR attestation required by the certifier.
5. Venue account identity, maximum notional, audit lineage, async fills, and
   ledger reconciliation gates are all green.

Destroy is intentionally not one command: EC2 termination protection and the
data volume's `prevent_destroy` guard require explicit operator decisions. This
prevents an accidental cleanup from erasing certification evidence.
