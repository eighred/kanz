"""Zero-result CodeQL gate; execution/upload authority belongs to the pinned Action.

Required rule IDs observed in actual clean 30449 PR merge analyses, CodeQL 2.27.4,
codeql query packs at 1be292a1e54696ca543b4ad0f9b4537b9687caa7. Rules live in
extensions, not driver.rules. Additions are allowed; removal requires review.
This is evidence binding, not proof against a dishonest compiler/workflow owner.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import urllib.error
import urllib.request

from release_ci import API, EvidenceError, NoRedirect

REQUIRED_RULES = {
    "actions": [
        "actions/artifact-poisoning/critical",
        "actions/cache-poisoning/code-injection",
        "actions/cache-poisoning/direct-cache",
        "actions/cache-poisoning/poisonable-step",
        "actions/code-injection/critical",
        "actions/envpath-injection/critical",
        "actions/envvar-injection/critical",
        "actions/excessive-secrets-exposure",
        "actions/improper-access-control",
        "actions/missing-workflow-permissions",
        "actions/secrets-in-artifacts",
        "actions/unmasked-secret-exposure",
        "actions/untrusted-checkout-toctou/critical",
        "actions/untrusted-checkout-toctou/high",
        "actions/untrusted-checkout/critical",
        "actions/untrusted-checkout/high",
        "actions/vulnerable-action"
    ],
    "go": [
        "go/allocation-size-overflow",
        "go/bad-redirect-check",
        "go/clear-text-logging",
        "go/command-injection",
        "go/constant-oauth2-state",
        "go/cookie-httponly-not-set",
        "go/cookie-secure-not-set",
        "go/disabled-certificate-check",
        "go/email-injection",
        "go/html-template-escaping-bypass-xss",
        "go/incomplete-hostname-regexp",
        "go/incomplete-url-scheme-check",
        "go/incorrect-integer-conversion",
        "go/insecure-hostkeycallback",
        "go/insecure-randomness",
        "go/insecure-tls",
        "go/missing-jwt-signature-check",
        "go/path-injection",
        "go/reflected-xss",
        "go/regex/missing-regexp-anchor",
        "go/request-forgery",
        "go/sql-injection",
        "go/stack-trace-exposure",
        "go/summary/lines-of-code",
        "go/suspicious-character-in-regex",
        "go/uncontrolled-allocation-size",
        "go/unsafe-quoting",
        "go/unsafe-unzip-symlink",
        "go/unvalidated-url-redirection",
        "go/weak-crypto-key",
        "go/weak-cryptographic-algorithm",
        "go/weak-sensitive-data-hashing",
        "go/xml/xpath-injection",
        "go/zipslip"
    ],
    "javascript-typescript": [
        "js/angular/disabling-sce",
        "js/angular/double-compilation",
        "js/angular/insecure-url-whitelist",
        "js/bad-code-sanitization",
        "js/bad-tag-filter",
        "js/biased-cryptographic-random",
        "js/build-artifact-leak",
        "js/case-sensitive-middleware-path",
        "js/clear-text-cookie",
        "js/clear-text-logging",
        "js/clear-text-storage-of-sensitive-data",
        "js/client-exposed-cookie",
        "js/client-side-unvalidated-url-redirection",
        "js/code-injection",
        "js/command-line-injection",
        "js/cors-misconfiguration-for-credentials",
        "js/cors-permissive-configuration",
        "js/cross-window-information-leak",
        "js/disabling-certificate-validation",
        "js/disabling-electron-websecurity",
        "js/double-escaping",
        "js/enabling-electron-insecure-content",
        "js/exposure-of-private-files",
        "js/functionality-from-untrusted-domain",
        "js/functionality-from-untrusted-source",
        "js/host-header-forgery-in-email-generation",
        "js/html-constructed-from-input",
        "js/identity-replacement",
        "js/incomplete-hostname-regexp",
        "js/incomplete-html-attribute-sanitization",
        "js/incomplete-multi-character-sanitization",
        "js/incomplete-sanitization",
        "js/incomplete-url-scheme-check",
        "js/incomplete-url-substring-sanitization",
        "js/incorrect-suffix-check",
        "js/insecure-dependency",
        "js/insecure-download",
        "js/insecure-helmet-configuration",
        "js/insecure-randomness",
        "js/insufficient-key-size",
        "js/insufficient-password-hash",
        "js/jwt-missing-verification",
        "js/loop-bound-injection",
        "js/missing-rate-limiting",
        "js/missing-token-validation",
        "js/overly-large-range",
        "js/path-injection",
        "js/polynomial-redos",
        "js/prototype-polluting-assignment",
        "js/prototype-pollution",
        "js/prototype-pollution-utility",
        "js/redos",
        "js/reflected-xss",
        "js/regex-injection",
        "js/request-forgery",
        "js/resource-exhaustion",
        "js/resource-exhaustion-from-deep-object-traversal",
        "js/second-order-command-line-injection",
        "js/sensitive-get-query",
        "js/server-crash",
        "js/server-side-unvalidated-url-redirection",
        "js/shell-command-constructed-from-input",
        "js/shell-command-injection-from-environment",
        "js/sql-injection",
        "js/stack-trace-exposure",
        "js/stored-xss",
        "js/summary/lines-of-code",
        "js/summary/lines-of-user-code",
        "js/system-prompt-injection",
        "js/tainted-format-string",
        "js/template-object-injection",
        "js/type-confusion-through-parameter-tampering",
        "js/unnecessary-use-of-cat",
        "js/unsafe-deserialization",
        "js/unsafe-dynamic-method-access",
        "js/unsafe-html-expansion",
        "js/unsafe-jquery-plugin",
        "js/unvalidated-dynamic-method-call",
        "js/useless-regexp-character-escape",
        "js/weak-cryptographic-algorithm",
        "js/xml-bomb",
        "js/xpath-injection",
        "js/xss",
        "js/xss-through-dom",
        "js/xss-through-exception",
        "js/xxe",
        "js/zipslip"
    ],
    "python": [
        "py/bad-tag-filter",
        "py/bind-socket-all-network-interfaces",
        "py/clear-text-logging-sensitive-data",
        "py/clear-text-storage-sensitive-data",
        "py/client-exposed-cookie",
        "py/code-injection",
        "py/command-line-injection",
        "py/cookie-injection",
        "py/csrf-protection-disabled",
        "py/flask-debug",
        "py/full-ssrf",
        "py/http-response-splitting",
        "py/incomplete-hostname-regexp",
        "py/incomplete-url-substring-sanitization",
        "py/insecure-cookie",
        "py/insecure-default-protocol",
        "py/insecure-protocol",
        "py/insecure-temporary-file",
        "py/ldap-injection",
        "py/nosql-injection",
        "py/overly-large-range",
        "py/pam-auth-bypass",
        "py/paramiko-missing-host-key-validation",
        "py/path-injection",
        "py/polynomial-redos",
        "py/redos",
        "py/reflective-xss",
        "py/regex-injection",
        "py/samesite-none-cookie",
        "py/sql-injection",
        "py/stack-trace-exposure",
        "py/summary/lines-of-code",
        "py/summary/lines-of-user-code",
        "py/template-injection",
        "py/unsafe-deserialization",
        "py/url-redirection",
        "py/use-of-input",
        "py/weak-crypto-key",
        "py/weak-cryptographic-algorithm",
        "py/weak-sensitive-data-hashing",
        "py/xml-bomb",
        "py/xpath-injection",
        "py/xxe"
    ]
}

MAX_BYTES = 32 << 20


class Refused(Exception):
    """Sanitized refusal; never emit SARIF content, source snippets or credentials."""


def require(condition, message):
    if not condition:
        raise Refused(message)


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "duplicate JSON key")
        result[key] = value
    return result


def decode(raw):
    require(len(raw) <= MAX_BYTES, "report exceeds byte bound")
    try:
        return json.loads(raw, object_pairs_hook=unique_object,
                          parse_constant=lambda _: (_ for _ in ()).throw(Refused("invalid JSON number")))
    except (ValueError, UnicodeError, RecursionError):
        raise Refused("malformed report") from None


def read_report(directory, language):
    require(sys.platform == "linux" and hasattr(os, "O_NOFOLLOW") and hasattr(os, "O_NONBLOCK"),
            "Linux no-follow/nonblocking report reader required")
    directory = Path(directory)
    require(not directory.is_symlink() and directory.is_dir(), "report directory unavailable")
    # One matrix language generates one report. Do not silently ignore extra reports.
    entries = []
    with os.scandir(directory) as listing:
        for entry in listing:
            require(not entries, "unexpected report inventory")
            entries.append(Path(entry.path))
    filename = "javascript" if language == "javascript-typescript" else language
    require(len(entries) == 1 and entries[0].name == filename + ".sarif", "unexpected report inventory")
    path = entries[0]
    require(not path.is_symlink(), "linked report refused")
    flags = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK
    fd = os.open(path, flags)
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        require(stat.S_ISREG(info.st_mode) and 0 < info.st_size <= MAX_BYTES, "invalid report size or type")
        raw = stream.read(MAX_BYTES + 1)
        require(len(raw) == info.st_size, "report changed during read")
    return decode(raw), hashlib.sha256(raw).hexdigest()


def report(value, language):
    require(isinstance(value, dict) and value.get("version") == "2.1.0", "unsupported SARIF")
    runs = value.get("runs")
    require(isinstance(runs, list) and len(runs) == 1 and isinstance(runs[0], dict), "unexpected SARIF runs")
    run = runs[0]
    require(run.get("results") == [] and isinstance(run.get("results"), list), "CodeQL results are not zero")
    require(not run.get("externalPropertyFileReferences"), "external report data refused")
    require(run.get("automationDetails", {}).get("id") == f"/language:{language}/", "wrong analysis category")
    tool = run.get("tool", {})
    driver = tool.get("driver", {})
    require(driver.get("name") == "CodeQL" and isinstance(driver.get("semanticVersion"), str)
            and re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?", driver["semanticVersion"]),
            "unexpected CodeQL tool")
    pack = "codeql/" + ("javascript" if language == "javascript-typescript" else language) + "-queries"
    extensions = tool.get("extensions")
    require(isinstance(extensions, list) and len(extensions) <= 64, "missing query packs")
    selected = [e for e in extensions if isinstance(e, dict) and e.get("name") == pack]
    require(len(selected) == 1, "missing or duplicate query pack")
    rules = selected[0].get("rules")
    require(isinstance(rules, list) and 0 < len(rules) <= 10000, "missing query rule inventory")
    ids = [r.get("id") if isinstance(r, dict) else None for r in rules]
    require(all(isinstance(i, str) and 0 < len(i) <= 512 for i in ids), "invalid query rule")
    require(len(set(ids)) == len(ids) and set(REQUIRED_RULES[language]) <= set(ids), "query coverage reduced")
    for extension in extensions:
        require(isinstance(extension, dict), "invalid query component")
        if extension is not selected[0]:
            require(not extension.get("rules"), "unexpected query rule component")
    require(not driver.get("rules"), "unexpected driver rule inventory")
    # GitHub's API projection omits invocation data. Never invent that evidence.
    # If local SARIF supplies it, every invocation must have succeeded.
    if "invocations" in run:
        invocations = run["invocations"]
        require(isinstance(invocations, list) and 0 < len(invocations) <= 32, "invalid invocation inventory")
        for invocation in invocations:
            require(isinstance(invocation, dict) and invocation.get("executionSuccessful") is True,
                    "unsuccessful query invocation")
            require(type(invocation.get("exitCode", 0)) is int and invocation.get("exitCode", 0) == 0, "query invocation exit failure")
            for key in ("toolExecutionNotifications", "toolConfigurationNotifications"):
                notes = invocation.get(key, [])
                require(isinstance(notes, list) and all(isinstance(n, dict) and n.get("level") != "error" for n in notes),
                        "query execution notification failure")
    return set(ids)


class GitHub:
    def __init__(self, token):
        require(bool(token), "GitHub read token missing")
        self.token = token
        self.opener = urllib.request.build_opener(NoRedirect())

    def get(self, path, sarif=False):
        request = urllib.request.Request(API + path, headers={
            "Authorization": "Bearer " + self.token,
            "Accept": "application/sarif+json" if sarif else "application/vnd.github+json",
            "X-GitHub-Api-Version": "2026-03-10",
        })
        try:
            with self.opener.open(request, timeout=30) as response:
                raw = response.read(MAX_BYTES + 1)
            return decode(raw)
        except (urllib.error.URLError, OSError, EvidenceError):
            raise Refused("uploaded analysis unavailable") from None


def verify_upload(api, sarif_id, language, sha, ref, local_rules):
    require(re.fullmatch(r"[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}", sarif_id), "invalid current upload ID")
    path = "/code-scanning/sarifs/" + sarif_id
    status = api.get(path)
    expected_url = API + "/code-scanning/analyses?sarif_id=" + sarif_id
    require(isinstance(status, dict) and status.get("processing_status") == "complete"
            and not status.get("errors") and status.get("analyses_url") == expected_url, "upload processing incomplete")
    analyses = api.get("/code-scanning/analyses?sarif_id=" + sarif_id + "&per_page=100&page=1")
    require(isinstance(analyses, list) and len(analyses) == 1, "unexpected uploaded analysis inventory")
    analysis = analyses[0]
    require(isinstance(analysis, dict), "invalid uploaded analysis")
    ident = analysis.get("id")
    require(type(ident) is int and ident > 0, "invalid analysis ID")
    endpoint = "/code-scanning/analyses/" + str(ident)
    require(analysis.get("url") == API + endpoint and analysis.get("sarif_id") == sarif_id
            and analysis.get("commit_sha") == sha and analysis.get("ref") == ref
            and analysis.get("analysis_key") == ".github/workflows/codeql.yml:analyze"
            and analysis.get("category") == "/language:" + language
            and analysis.get("error") == "" and analysis.get("warning") == ""
            and type(analysis.get("results_count")) is int and analysis["results_count"] == 0
            and type(analysis.get("rules_count")) is int and analysis["rules_count"] == len(local_rules),
            "uploaded analysis identity or verdict mismatch")
    env = decode(analysis.get("environment", "").encode())
    require(env == {"build-mode": "autobuild" if language == "go" else "none", "language": language},
            "unexpected uploaded query environment")
    uploaded = api.get(endpoint, sarif=True)
    require(report(uploaded, language) == local_rules, "local/uploaded query inventory mismatch")
    provenance = uploaded["runs"][0].get("versionControlProvenance")
    require(provenance == [{"branch": ref, "repositoryUri": "https://github.com/eighred/kanz", "revisionId": sha}],
            "uploaded source provenance mismatch")
    require(api.get(path) == status and api.get(endpoint) == analysis, "uploaded evidence changed")
    return ident


def source(env):
    require(env.get("GITHUB_ACTIONS") == "true" and env.get("GITHUB_REPOSITORY") == "eighred/kanz", "noncanonical workflow source")
    sha, ref = env.get("GITHUB_SHA", ""), env.get("GITHUB_REF", "")
    require(re.fullmatch(r"[0-9a-f]{40}", sha), "invalid source SHA")
    event = env.get("GITHUB_EVENT_NAME")
    require((event in {"push", "schedule", "workflow_dispatch"} and ref == "refs/heads/main")
            or (event == "pull_request" and re.fullmatch(r"refs/pull/[1-9][0-9]*/merge", ref)), "unexpected source ref")
    require(env.get("ANALYZE_OUTCOME") == "success" and not env.get("CODEQL_ACTION_EXTRA_OPTIONS"), "query execution not successful or customized")
    require(all(re.fullmatch(r"[1-9][0-9]*", env.get(k, "")) for k in ("GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT")), "invalid run identity")
    return sha, ref


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--language", choices=tuple(REQUIRED_RULES), required=True)
    args = parser.parse_args()
    try:
        sha, ref = source(os.environ)
        head = subprocess.run(["git", "rev-parse", "HEAD"], capture_output=True, text=True, check=True, timeout=10).stdout.strip()
        require(head == sha, "checkout/source mismatch")
        value, digest = read_report(os.environ.get("SARIF_DIRECTORY", ""), args.language)
        rules = report(value, args.language)
        ident = verify_upload(GitHub(os.environ.get("GH_TOKEN")), os.environ.get("SARIF_ID", ""), args.language, sha, ref, rules)
        print(f"CodeQL zero-result verdict: language={args.language} source={sha} run={os.environ['GITHUB_RUN_ID']} attempt={os.environ['GITHUB_RUN_ATTEMPT']} analysis={ident} report_sha256={digest} rules={len(rules)}")
        return 0
    except Refused as error:
        print(f"CodeQL verdict refused: {error}", file=sys.stderr)
        return 1
    except (OSError, ValueError, TypeError, KeyError, AttributeError, subprocess.SubprocessError):
        print("CodeQL verdict refused: incomplete or invalid analysis evidence", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
