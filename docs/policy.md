# Policy configuration and examples

`POLICY_FILE` selects an operator-owned JSON file loaded once at startup. Restart the proxy after editing it. See [the complete example](../examples/policy.json). Keep the file outside the agent's writable filesystem.

JSON is authoritative: remove `ALLOWED_ORGS`, `ALLOWED_USERS` and `BRANCH_PREFIX` from the process environment and `.env`. Setting a nonempty legacy policy variable alongside `POLICY_FILE` fails startup. Without `POLICY_FILE`, the existing environment configuration and non-GitHub tunnel mode continue working.

PAT/token files, `LISTEN_ADDR`, listener certificate paths, signing-key paths and `MAX_PUSH_BYTES` remain operational environment settings. Neither JSON nor `/status` contains credentials. Invalid/unknown fields, duplicate keys, null values, malformed rules and unreadable files fail startup. Empty lists are valid; at least one GitHub owner list must contain an owner.

## Destination rules

```json
{
  "https_mode": "intercept",
  "destinations": {
    "deny_hosts": ["blocked.example", "*.blocked.example", "192.168.20.12"],
    "deny_host_regexes": ["tracking[0-9]+\\.example"],
    "deny_ips": ["198.51.100.0/24", "2001:db8::/32"]
  },
  "github": {
    "allowed_users": ["jacobfg"],
    "branch_prefix": "agp/"
  }
}
```

Host rules examine the requested CONNECT hostname. Matching is case-insensitive for exact/wildcard rules because names are normalized to lowercase without a trailing dot. Use ASCII/punycode hostnames. Regexes use Go syntax, match the **whole normalized hostname**, and are case-sensitive unless explicitly using `(?i)`.

`*.example.com` matches any depth of subdomain, but not `example.com` itself. List both to block the apex and its subdomains. Ports and URL paths do not belong in host rules. Wildcards are supported only as a leading `*.`; regexes cover other patterns.

| Rule | Request host | Result |
| --- | --- | --- |
| `deny_hosts: ["blocked.example"]` | `BLOCKED.EXAMPLE.` | Block |
| Same | `sub.blocked.example` | Allow |
| `deny_hosts: ["*.blocked.example"]` | `a.b.blocked.example` | Block |
| Same | `blocked.example` or `notblocked.example` | Allow |
| Regex `tracking[0-9]+\.example` | `tracking12.example` | Block |
| Same | `nottracking12.example` | Allow |

A hostname block does not add that hostname's resolved addresses to an IP blacklist. Another hostname sharing the address remains allowed unless an IP rule blocks it.

### Literal host IP versus connection IP

```json
"destinations": {
  "deny_hosts": ["192.168.20.12"],
  "deny_ips": []
}
```

| Request | DNS answer | Result |
| --- | --- | --- |
| `https://192.168.20.12` | None needed | Block |
| `https://example.com` | `192.168.20.12` | Allow |

To block both, use:

```json
"destinations": {
  "deny_ips": ["192.168.20.12"]
}
```

IP rules inspect both literal destinations and DNS results. IPv4-mapped IPv6 addresses are normalized, so `::ffff:192.168.20.12` cannot bypass the IPv4 rule. IPv6 literals in `deny_hosts` are entered without URL brackets and compared in canonical form.

| `deny_ips` entry | Destination address | Result |
| --- | --- | --- |
| `198.51.100.0/24` | `198.51.100.9` | Block |
| Same | `198.51.101.9` | Allow |
| `2001:db8::/32` | `2001:db8::12` | Block |
| `192.168.20.12` | `::ffff:192.168.20.12` | Block |

The proxy resolves destinations itself. **If any DNS answer is blocked, it rejects the request**, even when another answer is allowed. It dials checked numeric addresses without a second DNS resolution, retaining the hostname for upstream TLS verification. Every new connection repeats this check. An existing connection remains connected to its already checked peer.

Destination rules apply in both modes, including GitHub metadata calls and reads. A blocked literal host may return 403 for CONNECT; a blocked resolved IP in interception mode returns 403 to the inner HTTPS request. Connections to local proxy ports 8443, 8444 and the configured listener port are always blocked to prevent proxy chaining.

These are destination controls, not content inspection: an allowed remote service that itself fetches other sites is still an allowed destination.

## GitHub write permissions

```json
"github": {
  "allowed_orgs": ["zendesk"],
  "allowed_users": ["jacobfg"],
  "branch_prefix": "agp/",
  "write_allow_repos": ["zendesk/repo-a", "zendesk/repo-b", "jacobfg/permission-test"],
  "write_deny_repos": ["zendesk/repo-b"]
}
```

| Write target | Result |
| --- | --- |
| `zendesk/repo-a` | Eligible |
| `zendesk/repo-b` | Block: deny wins |
| `zendesk/other` | Block: absent from allowlist |
| `jacobfg/permission-test` | Eligible |
| Any other owner | Block: owner not allowed |

“Eligible” still requires an explicitly supported operation, the branch prefix for branch/PR operations, and PAT-owner authorship for issue/PR edits. Cross-repository PR checks apply to both base and head repositories. GitHub's PAT permissions and repository rules also apply.

Repository rules accept **exact, case-insensitive `owner/repo` names only**. `zendesk/*` is invalid. A username belongs in `allowed_users`, not `allowed_orgs`; GitHub metadata determines its type.

| Configuration | Write effect |
| --- | --- |
| `write_allow_repos` omitted | No additional restriction beyond owner/operation rules |
| `write_allow_repos: []` | No repository writes |
| Nonempty allowlist | Only listed repositories are eligible |
| `write_deny_repos` omitted or `[]` | No additional denials |
| Repository appears in both | Denied |
| Either owner list omitted or `[]` | No owners of that type authorized |
| Both owner lists empty, or any list `null` | Startup error |

Repository rules never filter reads. Global destination blocks still apply. The proxy can narrow a PAT's write permissions but cannot grant private-repository read access absent from the PAT.

## TLS and trust

`https_mode` is required in JSON:

- `intercept`: terminate all CONNECT TLS, inspect HTTP authority and open verified upstream TLS. TLS 1.2 is the minimum on all three legs (client-to-proxy listener, inner client HTTPS, proxy-to-origin); TLS 1.3 is preferred.
- `tunnel`: retain opaque non-GitHub CONNECT forwarding. Destination rules apply, but **remote TLS versions cannot be guaranteed**. GitHub traffic is still intercepted and verified.

Interception requires `INTERCEPT_CA_CERT_FILE` and `INTERCEPT_CA_KEY_FILE`. Generate a separate CA with `just intercept-ca` or `github-agent-proxy init-ca --cert PATH --key PATH`. Existing files are never overwritten. Do not supply the mkcert root private key. Keep the signing key and PAT inaccessible to agents; distribute only public certificates.

The existing mkcert listener certificate can stay unchanged. Add the dedicated interception CA to the sandbox's public/corporate CA bundle and configure both destination and HTTPS-proxy trust (`SSL_CERT_FILE`, Git `http.sslCAInfo` / `http.proxySSLCAInfo`, curl `proxy-cacert`, Node `NODE_EXTRA_CA_CERTS`, Python `REQUESTS_CA_BUNDLE`). Go 1.27 supports `SSL_CERT_FILE` on macOS; older Go tools may need separate platform trust configuration.

Upstream verification uses the proxy process's system/corporate roots, independent of the certificates presented to clients. Missing trust, certificate pinning, unsupported client-certificate authentication, SNI/Host mismatches and non-HTTP traffic fail explicitly; interception never silently becomes a tunnel. HTTP/2, streaming and HTTP/1.1 WebSocket upgrades are supported. QUIC/UDP is blocked by the sandbox.

Forwarded requests preserve the client's User-Agent. If the client omits it or sends an empty value, the proxy sends no User-Agent header, including no Go transport default. Only proxy-initiated GitHub identity and ownership lookups use `User-Agent: github-agent-proxy`.

## Trial on the native Mac proxy

The native trial preserves corporate/VPN routing and leaves the existing 8443 service available:

```sh
just trial-start
just trial-sandbox /Users/your-user/src/agent-safehouse-custom
just trial-status
/Users/your-user/src/agent-safehouse-custom/run-in-sandbox-trial gh api user
/Users/your-user/src/agent-safehouse-custom/run-in-sandbox-trial gh repo view
```

`trial-start` builds into `~/.config/github-agent-proxy-trial-private`, generates the dedicated CA once, and launches `local.github-agent-proxy-trial` on `127.0.0.1:8444`. It initializes JSON using the existing native service's owner/branch settings, retaining any existing trial policy on subsequent starts. The initial deny rules cover `blocked.example` and documentation-only IP ranges. Edit the trial policy to set repository restrictions, then restart with `just trial-start`.

Public trust and CLI configuration live under `~/.config/agent-safehouse-trial`. Set `TRIAL_BASE_CA_BUNDLE` if the existing corporate/public/listener bundle is not `~/.nscacert_custom_combined.pem`. The new CA is not installed in the host Keychain. The companion installer preserves local launcher changes and adds an opt-in `--proxy-trial` selector plus `run-in-sandbox-trial`; the original launcher continues to use 8443. The trial profile allows only outbound TCP to 127.0.0.1:8444 and denies access to both private proxy directories.

Rollback: use the original `run-in-sandbox` and run `just trial-stop`. Stopping the trial does not stop the existing service. Promotion is a separate operator action after successful smoke tests; these commands do not promote automatically.

For Apple Container, keep JSON and CA files under `PROXY_PRIVATE`, set their absolute host paths in `.env`, remove legacy policy variables and use `just run` or `just test-live`. The recipes translate paths into the read-only `/run/proxy` mount. The combined test container contains the PAT and is a development tool, not the isolation boundary.

## Validation and diagnostics

`GET /status` on the loopback listener reports effective policy, mode, minimum TLS and the interception CA's SHA-256 fingerprint. `all_https_tls_enforced` distinguishes full interception from legacy tunnelling. A `null` repository allowlist in status means it was omitted in the input; status is diagnostic output, not a reusable configuration file. `/healthz` is local liveness and makes no GitHub requests.

Run `go test -race -count=1 -timeout=3m ./...`, `go vet ./...`, and `python3 -m unittest discover -s scripts -p 'test_*.py'`. `just test` runs Go and real gh/git integration tests against local mocks in the test container, without a PAT. `just policy OWNER/REPO Organization` (or `User`) reports allow/block results using the configured JSON or legacy environment settings; it mounts only the JSON file, never private credentials.

The documented hostname/IP examples are covered by `TestDestinationExamples` and `TestCheckedDialDNS`; the complete example is loaded by `TestDocumentedPolicyFile`. Repository behavior is covered by `TestRepositoryPolicyAcrossAPIs`, `TestCrossRepositoryPRPolicy` and real CLI tests. TLS, HTTP/2, streaming, upgrades, certificate expiry and shutdown have local TLS integration tests. Live validation should use `jacobfg/permission-test` for any writes, with unique `agp/` branch names and test-issue cleanup.

For repeatable read-only live checks (including optional Claude authentication):

```sh
python3 scripts/live-smoke.py /path/to/agent-safehouse-custom \
  --repo OWNER/REPO --workdir /path/to/local/checkout --claude
```

This checks repo detection, Git reads, public HTTPS and the trial's initial destination blocks. It does not create issues or push branches. A PAT that cannot read `jacobfg/permission-test` cannot perform live write tests there; use an authorized test PAT for that final check. Helper output is suppressed.
