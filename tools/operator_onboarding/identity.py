"""Install the disposable authority used by the operator browser qualification."""
import base64
import hashlib
import hmac
import re
import secrets


NAMESPACE = "kanz-services"
ORIGIN = "https://localhost:18443"
PG_IMAGE = "postgres:16-alpine@sha256:1fb72075a1de85a1d39de23c2da81baef1b71bf5c0b61a14cbbfdb072a6246d9"


def verifier(password):
    """PostgreSQL accepts SCRAM verifiers when creating a role without exposing its password in SQL."""
    salt = secrets.token_bytes(16)
    salted = hashlib.pbkdf2_hmac("sha256", password.encode("ascii"), salt, 4096)
    stored = hashlib.sha256(hmac.digest(salted, b"Client Key", "sha256")).digest()
    server = hmac.digest(salted, b"Server Key", "sha256")
    def b64(value):
        return base64.b64encode(value).decode("ascii")
    return f"SCRAM-SHA-256$4096:{b64(salt)}${b64(stored)}:{b64(server)}"


def install(k, apply, image, control_node):
    if not re.fullmatch(r"kanz77-[a-z0-9]+-control", control_node):
        raise ValueError("identity requires the disposable control node")
    issuer = "https://identity." + control_node + ".invalid"
    audience = "kanz-api"
    address = "http://identity.kanz-services.svc:8087"
    subject = "onboarding-" + secrets.token_hex(8)

    def meta(name):
        return {"name": name, "namespace": NAMESPACE}

    def obj(kind, name, spec):
        return {"apiVersion": "v1", "kind": kind, "metadata": meta(name), "spec": spec}

    def value(name, text):
        return {"name": name, "value": text}

    def secret_env(name, secret):
        return {"name": name, "valueFrom": {"secretKeyRef": {"name": secret, "key": "value"}}}

    def create_secret(name, text):
        apply({"apiVersion": "v1", "kind": "Secret", "metadata": meta(name), "type": "Opaque",
               "stringData": {"value": text}})

    def pod(name, containers, **kwargs):
        doc = obj("Pod", name, {"nodeName": control_node, "automountServiceAccountToken": False,
                                "restartPolicy": "Never", "containers": containers, **kwargs})
        doc["metadata"]["labels"] = {"app": name}
        apply(doc)

    def service(name, port):
        apply(obj("Service", name, {"selector": {"app": name}, "ports": [{"port": port, "targetPort": port}]}))

    def ready(name):
        k("-n", NAMESPACE, "wait", "--for=condition=Ready", "pod/" + name, "--timeout=180s")

    def pg(sql, database):
        return k("-n", NAMESPACE, "exec", "-i", "identity-db", "--", "sh", "-ceu",
                 'export PGPASSWORD="$POSTGRES_PASSWORD"; exec psql -X -w -qAt -v ON_ERROR_STOP=1 '
                 '-h 127.0.0.1 -U postgres -d "$1"', "psql", database, data=sql)

    app_password = secrets.token_hex(32)
    migration_password = secrets.token_hex(32)
    create_secret("identity-db-admin", secrets.token_hex(32))
    for role, password in (("app", app_password), ("migrate", migration_password)):
        create_secret("identity-" + role + "-dsn",
                      f"postgresql://identity_{role}:{password}@identity-db:5432/identity?sslmode=disable")

    service("identity-db", 5432)
    pod("identity-db", [{"name": "postgres", "image": PG_IMAGE,
                         "env": [secret_env("POSTGRES_PASSWORD", "identity-db-admin"),
                                 value("POSTGRES_INITDB_ARGS", "--auth-local=scram-sha-256 --auth-host=scram-sha-256")],
                         "volumeMounts": [{"name": "db", "mountPath": "/var/lib/postgresql/data"}],
                         "readinessProbe": {"exec": {"command": ["pg_isready", "-h", "127.0.0.1", "-U", "postgres"]},
                                            "periodSeconds": 2}}],
        volumes=[{"name": "db", "emptyDir": {"medium": "Memory", "sizeLimit": "512Mi"}}])
    ready("identity-db")
    pg(f"""CREATE ROLE identity_app LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE
           CONNECTION LIMIT 20 PASSWORD '{verifier(app_password)}';
           CREATE ROLE identity_migrate LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE
           CONNECTION LIMIT 4 PASSWORD '{verifier(migration_password)}';
           CREATE DATABASE identity OWNER identity_migrate;""", "postgres")
    posture = pg("""REVOKE ALL ON DATABASE identity FROM PUBLIC;
        GRANT CONNECT ON DATABASE identity TO identity_app;
        REVOKE CREATE ON SCHEMA public FROM PUBLIC;
        GRANT USAGE ON SCHEMA public TO identity_app;
        ALTER DEFAULT PRIVILEGES FOR ROLE identity_migrate IN SCHEMA public
          GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO identity_app;
        ALTER DEFAULT PRIVILEGES FOR ROLE identity_migrate IN SCHEMA public
          GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO identity_app;
        SELECT count(*) FROM pg_roles WHERE rolname IN ('identity_app','identity_migrate')
          AND rolcanlogin AND NOT (rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole OR rolreplication);
        SELECT has_schema_privilege('identity_app', 'public', 'CREATE');""", "identity")
    if posture.split() != ["2", "f"]:
        raise ValueError("identity database role posture failed")

    security = {"allowPrivilegeEscalation": False, "runAsNonRoot": True, "runAsUser": 65532,
                "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"]}}
    common = {"image": image, "imagePullPolicy": "Never", "securityContext": security}
    migrate = {**common, "name": "migrate", "command": ["/migrate", "--dir", "/migrations/identity"],
               "env": [secret_env("KANZ_MIGRATE_DATABASE_URL", "identity-migrate-dsn")]}
    identity = {**common, "name": "identity", "command": ["/identity"],
                "env": [secret_env("IDENTITY_DATABASE_URL", "identity-app-dsn"),
                        value("IDENTITY_TOKEN_ISSUER", issuer), value("IDENTITY_TOKEN_AUDIENCE", audience),
                        value("IDENTITY_ALLOW_EPHEMERAL_KEY", "true"), value("IDENTITY_MFA_ENABLED", "true"),
                        value("IDENTITY_MFA_REQUIRED", "true"), value("IDENTITY_WEBAUTHN_RP_ID", "localhost"),
                        value("IDENTITY_WEBAUTHN_ORIGIN", ORIGIN), value("IDENTITY_ADMIN_ROLE", "kanz-identity-admin")],
                "readinessProbe": {"httpGet": {"path": "/readyz", "port": 8087}, "periodSeconds": 2}}
    service("identity", 8087)
    pod("identity", [identity], initContainers=[migrate])
    ready("identity")
    # exec's private stdout returns the invitation to the parent. A Pod/Job entrypoint
    # would instead retain the one-use invitation in the Kubernetes container log.
    invite = k("-n", NAMESPACE, "exec", "identity", "-c", "identity", "--", "/invite",
               "-by", "disposable-estate-bootstrap", "-subject", subject, "-tenant", "onboarding",
               "-roles", "kanz-user,kanz-operator", "-ttl", "15m").strip()
    if not re.fullmatch(r"[A-Za-z0-9_-]{43}", invite):
        raise ValueError("identity did not return a single bootstrap invitation")
    return {"issuer": issuer, "audience": audience, "jwks_uri": address + "/jwks.json",
            "revocations_uri": address + "/revocations", "identity_url": address, "invite": invite,
            "subject": subject, "origin": ORIGIN}
