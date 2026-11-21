"""Physical recovery for the disposable local custody pilot (PostgreSQL 16)."""

import re
import secrets
import subprocess
import time


class CustodyPITR:
    def __init__(self, docker, env, image):
        if (len(docker) != 3 or docker[:2] != ["docker", "--host"]
                or not docker[2].startswith(("unix:///", "npipe:////./pipe/"))
                or not re.fullmatch(r"postgres@sha256:[0-9a-f]{64}", image)):
            raise ValueError("A local Docker engine and digest-pinned PostgreSQL image are required.")
        self.docker = docker
        self.env = env
        self.image = image
        nonce = secrets.token_hex(16)
        self.volume = "kanz-custody-pitr-" + nonce
        self.marker = "custody_close_" + nonce
        self.label = "kanz.custody-pitr"
        self.target = None
        self.primary = None
        self.evidence = {}

    def _run(self, args, timeout=30, check=True):
        result = subprocess.run([*self.docker, *args], env=self.env, capture_output=True,
                                text=True, timeout=timeout, check=False)
        if check and result.returncode:
            raise RuntimeError(f"PITR Docker {args[0]} failed (exit {result.returncode}).")
        return result

    def __enter__(self):
        self._run(["volume", "create", "--label", f"{self.label}={self.marker}", self.volume])
        self._check_volume()
        return self

    def _check_volume(self):
        label = self._run(["volume", "inspect", "--format",
                           '{{index .Labels "' + self.label + '"}}', self.volume]).stdout.strip()
        if label != self.marker:
            raise RuntimeError("PITR volume ownership could not be verified.")

    def __exit__(self, *_):
        try:
            if self.target:
                self._run(["rm", "--force", self.target])
        finally:
            self._check_volume()
            self._run(["volume", "rm", self.volume])

    def primary_args(self):
        return ["--mount", f"type=volume,src={self.volume},dst=/pitr,volume-nocopy",
                "--label", f"{self.label}={self.marker}"]

    @staticmethod
    def primary_command():
        return (
            "set -eu; umask 077; head -c 32 /dev/urandom | base64 > /run/pilot/admin; "
            "mkdir /pitr/wal; chown postgres:postgres /run/pilot/admin /pitr /pitr/wal; "
            "exec docker-entrypoint.sh postgres -c archive_mode=on "
            "-c \"archive_command=test -e /pitr/wal/%f && cmp -s %p /pitr/wal/%f "
            "|| (test ! -e /pitr/wal/%f && cp %p /pitr/wal/%f.partial && mv /pitr/wal/%f.partial /pitr/wal/%f)\""
        )

    def _sql(self, container, query, check=True):
        return self._run(["exec", "--user", "postgres", container, "psql", "-XqAtw",
                          "-h", "/var/run/postgresql", "-d", "postgres", "-v", "ON_ERROR_STOP=1",
                          "-c", query], check=check)

    def base_backup(self, primary_id):
        if self.primary is not None or not re.fullmatch(r"[a-f0-9]{64}", primary_id):
            raise ValueError("Expected the full ID of the new pilot primary.")
        fmt = ('{{index .Config.Labels "' + self.label + '"}}|{{.Config.Image}}|'
               '{{range .Mounts}}{{if eq .Destination "/pitr"}}{{.Name}}:{{.RW}}{{end}}{{end}}')
        identity = self._run(["inspect", "--format", fmt, primary_id]).stdout.strip()
        if identity != f"{self.marker}|{self.image}|{self.volume}:true":
            raise RuntimeError("Refusing backup outside the current PITR fixture.")
        source = self._sql(primary_id, "SELECT system_identifier, "
                           "current_setting('server_version_num')::int / 10000, pg_is_in_recovery(), "
                           "current_setting('archive_mode') FROM pg_control_system()").stdout.strip().split("|")
        if len(source) != 4 or not source[0].isdigit() or source[1:] != ["16", "f", "on"]:
            raise RuntimeError("Expected an archiving PostgreSQL 16 primary.")
        self._run(["exec", "--user", "postgres", primary_id, "pg_basebackup", "-h", "/var/run/postgresql",
                   "-U", "postgres", "-w", "-D", "/pitr/base", "-X", "stream", "--checkpoint=fast",
                   "--manifest-checksums=SHA256"], timeout=120)
        self._run(["exec", "--user", "postgres", primary_id, "pg_verifybackup", "/pitr/base"], timeout=120)
        bounds = self._sql(primary_id, """
            WITH manifest AS (SELECT pg_read_file('/pitr/base/backup_manifest')::jsonb->'WAL-Ranges' AS ranges)
            SELECT ranges->0->>'Timeline', ranges->0->>'Start-LSN', ranges->0->>'End-LSN'
            FROM manifest WHERE jsonb_array_length(ranges) = 1
        """).stdout.strip().split("|")
        if len(bounds) != 3 or not bounds[0].isdigit() or not all(re.fullmatch(r"[0-9A-F]+/[0-9A-F]+", v) for v in bounds[1:]):
            raise RuntimeError("Invalid WAL range in the verified physical backup.")
        self.primary = primary_id
        self.evidence.update(system_identifier=source[0], timeline=int(bounds[0]), base_start_lsn=bounds[1],
                             base_end_lsn=bounds[2], base_backup_verified=True)

    def mark(self, primary_id):
        if self.primary != primary_id or not self.primary or "marker_lsn" in self.evidence:
            raise RuntimeError("A fresh verified backup of this primary is required.")
        lsn = self._sql(primary_id, f"SELECT pg_create_restore_point('{self.marker}')").stdout.strip()
        if not re.fullmatch(r"[0-9A-F]+/[0-9A-F]+", lsn):
            raise RuntimeError("Invalid restore point LSN.")
        segment = self._sql(primary_id, f"SELECT pg_walfile_name('{lsn}'::pg_lsn) "
                            f"WHERE '{lsn}'::pg_lsn > '{self.evidence['base_end_lsn']}'::pg_lsn").stdout.strip()
        if not re.fullmatch(r"[0-9A-F]{24}", segment) or int(segment[:8], 16) != self.evidence["timeline"]:
            raise RuntimeError("Restore point is outside the backup timeline or precedes backup completion.")
        self._archive_segment(primary_id, segment)
        self.evidence.update(marker=self.marker, marker_lsn=lsn, archived_segment=segment)

    def _archive_segment(self, primary_id, segment):
        self._sql(primary_id, "SELECT pg_switch_wal()")
        # A .done receipt is written by PostgreSQL itself only after archive_command succeeds.
        query = (f"SELECT (pg_stat_file('pg_wal/archive_status/{segment}.done', true)).size IS NOT NULL "
                 f"AND (pg_stat_file('/pitr/wal/{segment}', true)).size = pg_size_bytes(current_setting('wal_segment_size'))")
        deadline = time.monotonic() + 30
        while self._sql(primary_id, query).stdout.strip() != "t":
            if time.monotonic() >= deadline:
                raise RuntimeError("PostgreSQL did not confirm archiving the restore point segment.")
            time.sleep(0.2)

    def archive_after_target(self, primary_id):
        if not self.primary or primary_id != self.primary or "marker_lsn" not in self.evidence:
            raise RuntimeError("Expected the marked fixture and its later committed writes.")
        later = self._sql(primary_id, "SELECT pg_current_wal_flush_lsn(), pg_walfile_name(pg_current_wal_flush_lsn()) "
                          f"WHERE pg_current_wal_flush_lsn() > '{self.evidence['marker_lsn']}'::pg_lsn").stdout.strip().split("|")
        if (len(later) != 2 or not re.fullmatch(r"[0-9A-F]+/[0-9A-F]+", later[0])
                or not re.fullmatch(r"[0-9A-F]{24}", later[1]) or later[1] <= self.evidence["archived_segment"]
                or int(later[1][:8], 16) != self.evidence["timeline"]):
            raise RuntimeError("Later committed data must reach a WAL segment beyond the restore point.")
        self._archive_segment(primary_id, later[1])
        self.evidence.update(post_target_flush_lsn=later[0], post_target_archived_segment=later[1])

    def restore(self):
        if self.target or "marker_lsn" not in self.evidence:
            raise RuntimeError("An archived restore point is required for a fresh recovery.")
        started = time.monotonic()
        # Root copies the verified physical image onto independent tmpfs before the image's
        # normal entrypoint changes user. The base backup and WAL stay read-only.
        command = (
            'set -eu; cp -a /pitr/base/. "$PGDATA/"; touch "$PGDATA/recovery.signal"; '
            'chown postgres:postgres "$PGDATA" "$PGDATA/recovery.signal"; chmod 0700 "$PGDATA"; '
            'exec docker-entrypoint.sh postgres -c archive_mode=off -c primary_conninfo= '
            '-c hot_standby=on -c "restore_command=cp /pitr/wal/%f %p" '
            f'-c recovery_target_name={self.marker} -c recovery_target_timeline={self.evidence["timeline"]} '
            '-c recovery_target_action=pause'
        )
        self.target = self._run(["run", "--detach", "--pull=never", "--name", self.volume + "-restore",
                                 "--label", f"{self.label}={self.marker}", "--publish", "127.0.0.1::5432",
                                 "--tmpfs", "/var/lib/postgresql/data", "--mount",
                                 f"type=volume,src={self.volume},dst=/pitr,readonly,volume-nocopy",
                                 "--entrypoint", "sh", self.image, "-ec", command]).stdout.strip()
        if not re.fullmatch(r"[0-9a-f]{64}", self.target):
            self.target = None
            raise RuntimeError("Docker did not return a recovery container ID.")
        deadline = started + 60
        while not self._paused():
            if time.monotonic() >= deadline or self._run(["inspect", "--format", "{{.State.Running}}",
                                                         self.target]).stdout.strip() != "true":
                raise RuntimeError("Recovery did not pause at the archived restore point.")
            time.sleep(0.2)
        endpoint = self._run(["port", self.target, "5432/tcp"]).stdout.strip()
        if not re.fullmatch(r"127\.0\.0\.1:[1-9][0-9]{0,4}", endpoint) or int(endpoint.rsplit(":", 1)[1]) > 65535:
            raise RuntimeError("Recovery must expose one literal loopback endpoint.")
        self.evidence.update(replay_lsn=self.evidence["marker_lsn"], recovery_paused=True,
                             restore_seconds=round(time.monotonic() - started, 3))
        return self.target, endpoint

    def _paused(self):
        query = ("SELECT system_identifier, pg_is_in_recovery(), pg_get_wal_replay_pause_state(), "
                 "pg_last_wal_replay_lsn(), current_setting('recovery_target_name'), "
                 "current_setting('recovery_target_timeline'), current_setting('transaction_read_only') "
                 "FROM pg_control_system()")
        result = self._sql(self.target, query, check=False)
        return result.returncode == 0 and result.stdout.strip().split("|") == [
            self.evidence["system_identifier"], "t", "paused", self.evidence["marker_lsn"],
            self.marker, str(self.evidence["timeline"]), "on"]

    def verify_paused(self):
        if not self.target or not self._paused():
            raise RuntimeError("Recovered cluster no longer verifies at the named restore point.")
