"""Linux executable refusal tests for the isolated restored-state helper."""
import datetime
import json
import os
from pathlib import Path
import shutil
import subprocess
import unittest

HELPER = Path(__file__).with_name("tv-sync-restored-proof.sh")


@unittest.skipUnless(shutil.which("bash") and shutil.which("jq"), "bash and jq required")
class RestoredProofTests(unittest.TestCase):
    def run_helper(self, script, stdin=""):
        result = subprocess.run([shutil.which("bash"), "-ceu", 'source "$HELPER"\necho HELPER_LOADED >&2\n' + script], input=stdin, text=True,
                              capture_output=True, timeout=10,
                              env={**os.environ, "HELPER": str(HELPER)})
        self.assertIn("HELPER_LOADED", result.stderr, result.stderr)
        return result

    def proof(self):
        return dict(schema_version=1, status="VERIFIED", source_version="v9.0.0",
                    tenant_sha256="a" * 64, retention_seconds=604800, fact_count=3,
                    checkpoint_seq=1, watermark=3, input_sha256="b" * 64,
                    snapshot_sha256="c" * 64, state_sha256="d" * 64,
                    checked_at="2026-11-06T00:00:00.123Z")

    def validate(self, proof):
        epoch = int(datetime.datetime(2026, 11, 6, tzinfo=datetime.timezone.utc).timestamp())
        return self.run_helper(f'TV_VERSION=v9.0.0; proof=$(cat); validate_tv_restore_result "$proof" "{ "a" * 64 }" {epoch} {epoch + 2}', json.dumps(proof))

    def test_valid_exact_receipt(self):
        self.assertEqual(self.validate(self.proof()).returncode, 0)

    def test_counter_types_and_bounds_refuse(self):
        for field in ("fact_count", "checkpoint_seq", "watermark"):
            for value in ("3", 1.5, -1, 9007199254740992, None, True):
                with self.subTest(field=field, value=value):
                    proof = self.proof(); proof[field] = value
                    self.assertNotEqual(self.validate(proof).returncode, 0)

    def test_receipt_identity_history_and_freshness_refuse(self):
        for field, value in (("source_version", "v8.0.0"), ("tenant_sha256", "b" * 64),
                             ("fact_count", 0), ("checkpoint_seq", 0), ("watermark", 0),
                             ("checked_at", "2020-01-01T00:00:00Z"),
                             ("checked_at", "2026-11-06T00:00:03Z"),
                             ("checked_at", "2026-02-30T00:00:00Z"),
                             ("checked_at", "not-a-timestamp"), ("extra", 1)):
            with self.subTest(field=field, value=value):
                proof = self.proof(); proof[field] = value
                self.assertNotEqual(self.validate(proof).returncode, 0)

    def test_manifest_credential_isolation_and_platform(self):
        result = self.run_helper('DRILL_NS=kanz-recovery-drill; DRILL_UID=fixture; TV_RUNTIME_IMAGE=example@sha256:abc; TV_VERSION=v9.0.0; tv_restore_job_manifest')
        self.assertEqual(result.returncode, 0, result.stderr)
        pod = json.loads(result.stdout)["spec"]["template"]["spec"]
        self.assertFalse(pod["automountServiceAccountToken"])
        self.assertEqual(pod["nodeSelector"]["kubernetes.io/arch"], "amd64")
        runtime, init = pod["containers"][0], pod["initContainers"][0]
        self.assertEqual(runtime["image"], "example@sha256:abc")
        self.assertNotIn("owner-credentials", [m["name"] for m in runtime["volumeMounts"]])
        self.assertIn("postgres-ca", [m["name"] for m in init["volumeMounts"]])
        script = init["command"][2]
        self.assertLess(script.index('[[ "$logging" == t ]]'), script.index('CREATE ROLE'))
        self.assertIn("PGSSLMODE=verify-full", script)
        self.assertIn("PGSSLROOTCERT=/run/postgres-ca/ca.crt", script)

    def test_unsafe_server_logging_refuses_before_password_generation(self):
        result = self.run_helper('''role_script=$(tv_restore_role_script)
 cat() { case "$1" in */host) echo kanz-postgres-drill-rw;; */password) echo synthetic-owner;; *) return 99;; esac; }
 psql() { echo f; }
 od() { echo PASSWORD_GENERATION_REACHED >&2; return 99; }
 eval "$role_script"''')
        self.assertEqual(result.returncode, 64)
        self.assertNotIn("PASSWORD_GENERATION_REACHED", result.stderr)

    def test_cleanup_binds_uid_and_verifies_storage_absence(self):
        result = self.run_helper('''DRILL_NS=kanz-recovery-drill; TV_OWNER_NONCE=fixture; DRILL_UID=11111111-1111-1111-1111-111111111111
 state=$(mktemp); echo present > "$state"; trap 'rm -f "$state"' EXIT
 tvk() { case "$*" in
  "get namespace "*) if [[ $(cat "$state") == present ]]; then printf '%s' '{"metadata":{"uid":"11111111-1111-1111-1111-111111111111"}}'; fi;;
  "get persistentvolumes "*|*"get persistentvolumeclaims "*) printf '%s' '{"items":[]}';;
  "delete --raw "*) jq -e '.preconditions.uid=="11111111-1111-1111-1111-111111111111"' >/dev/null || return 99; echo absent > "$state";;
  *) return 99;; esac; }
 cleanup_tv_restore_namespace
 [[ -z "$DRILL_UID" && -z "$TV_OWNER_NONCE" ]]''')
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_preexisting_namespace_never_mutates(self):
        result = self.run_helper('''DRILL_NS=kanz-recovery-drill
 die() { exit 64; }
 tvk() { case "$*" in
   "get storageclass "*) printf '%s' '{"provisioner":"rancher.io/local-path","reclaimPolicy":"Delete","volumeBindingMode":"WaitForFirstConsumer"}';;
   "get namespace "*) printf '%s' '{"metadata":{"uid":"foreign"}}';;
   *) echo MUTATION; exit 99;; esac; }
 begin_tv_restore_namespace''')
        self.assertEqual(result.returncode, 64)
        self.assertNotIn("MUTATION", result.stdout)

    def test_orphan_prior_volume_refuses_before_namespace_creation(self):
        result = self.run_helper('''DRILL_NS=kanz-recovery-drill
 die() { exit 64; }
 tvk() { case "$*" in
  "get storageclass "*) printf '%s' '{"provisioner":"rancher.io/local-path","reclaimPolicy":"Delete","volumeBindingMode":"WaitForFirstConsumer"}';;
  "get namespace "*) return 0;;
  "get persistentvolumes "*) printf '%s' '{"items":[{"spec":{"claimRef":{"namespace":"kanz-recovery-drill"}}}]}';;
  *) echo MUTATION; return 99;; esac; }
 begin_tv_restore_namespace''')
        self.assertEqual(result.returncode, 64)
        self.assertNotIn("MUTATION", result.stdout)

    def test_unknown_cleanup_inventory_does_not_delete(self):
        result = self.run_helper('''DRILL_NS=kanz-recovery-drill; TV_OWNER_NONCE=fixture; DRILL_UID=fixture
 tvk() { case "$*" in "get namespace "*) return 1;; *) echo MUTATION; return 99;; esac; }
 cleanup_tv_restore_namespace''')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("MUTATION", result.stdout)

    def test_absent_namespace_with_retained_volume_refuses_cleanup(self):
        result = self.run_helper('''DRILL_NS=kanz-recovery-drill; TV_OWNER_NONCE=fixture; DRILL_UID=fixture
 tvk() { case "$*" in "get namespace "*) return 0;; "get persistentvolumes "*) printf '%s' '{"items":[{"spec":{"claimRef":{"namespace":"kanz-recovery-drill"}}}]}';; *) echo MUTATION; return 99;; esac; }
 cleanup_tv_restore_namespace''')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("MUTATION", result.stdout)


if __name__ == "__main__":
    unittest.main()
