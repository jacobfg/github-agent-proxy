# GitHub agent proxy

A Go proxy that keeps a fine-grained GitHub PAT outside a coding agent's environment and applies a narrower write policy. Normal `gh` and `git` commands use a local TLS endpoint. For GitHub, client credentials are discarded and the proxy injects its own token only into requests to fixed GitHub upstreams. With a JSON policy, the proxy can intercept all HTTPS, enforce TLS 1.2+, block destinations by hostname/IP/CIDR, and narrow GitHub writes by repository. Legacy configuration keeps opaque non-GitHub CONNECT tunnels. See the [policy reference and examples](docs/policy.md).

GitHub reads have no repository-owner, author, or branch restrictions. GitHub still applies the PAT's own permissions. GitHub writes are denied unless explicitly supported below.

## Policy

An allowed repository owner is an organisation listed in `ALLOWED_ORGS` or a personal account listed in `ALLOWED_USERS` (or their JSON equivalents). Either or both lists may be empty; two empty lists deny every GitHub write. GitHub's owner type must match the list: putting a username in `ALLOWED_ORGS` does not allow its personal repositories. JSON repository allow/deny rules further restrict every write below.

| Operation | Allowed when |
| --- | --- |
| REST GET/HEAD, GraphQL queries, Git clone/fetch, LFS downloads, Markdown rendering | No resource filtering |
| Create issue | Repository has an allowed owner |
| Edit/close/reopen/delete issue | Allowed repository owner; author is the PAT owner; deletion uses GraphQL |
| Create PR | Base and head repositories have allowed owners; head branch matches the prefix |
| Edit/close/reopen PR, draft/ready state | Allowed base and head repository owners; author is the PAT owner; head branch matches the prefix |
| Add issue/PR comment | Parent issue/PR passes the owner and repository checks above |
| Edit/delete issue comment through REST | Comment and parent belong to the PAT owner; parent passes the checks above |
| Change issue/PR labels, assignees, milestone | Existing issue/PR passes the checks above |
| Git push, including force and deletion | Allowed repository owner; every command targets `refs/heads/<prefix>…` |
| REST/GraphQL ref creation, update, deletion | Allowed repository owner; branch ref matches the prefix |
| REST contents writes/deletes | Allowed repository owner; explicit `branch` matches the prefix |
| REST branch rename | Allowed repository owner; both old and new names match the prefix |
| Anything else | Denied |

Issues have no branch, so their writes use the repository-owner and author rules. Allowing a personal account does not permit editing issues or PRs authored by someone other than the PAT owner. A PR's base may be `main`: creating or editing the PR does not write that branch. Merging, auto-merge, and updating a PR branch through GitHub's merge API are denied. Push the agent branch explicitly instead.

Repository creation/deletion/settings, Actions dispatches, releases, tags, reviews, issue transfers, project mutations, standalone Git object creation, LFS uploads, signed pushes, and push options are not allowlisted. Unknown fields on allowed write operations are also denied. Cross-repository PR creation is supported through GraphQL with `headRepositoryId`; REST creation is limited to the same repository.

The proxy resolves its identity with `/user` at startup. Each write obtains current repository/resource metadata before forwarding. Lookup failures deny the write. GraphQL aliases, fragments, variable defaults, and every field in a mutation are checked. All Git ref commands are checked before any receive-pack bytes are sent upstream; the pack itself streams after validation. Pushes from shallow clones are supported: leading `shallow <commit-id>` history-boundary declarations are preserved, and every ref update must still pass the branch policy.

## Install with Homebrew

The repository also contains a Homebrew tap. gobottle builds Apple Silicon bottles, and native macOS CI verifies installation. The package contains only the binary; your trusted sandbox launcher supplies the policy and owns proxy startup and cleanup. See [Homebrew installation and releases](docs/homebrew.md).

```sh
brew tap jacobfg/github-agent-proxy https://github.com/jacobfg/github-agent-proxy
brew trust --formula jacobfg/github-agent-proxy/github-agent-proxy # Skip on older Homebrew without trust.
brew install jacobfg/github-agent-proxy/github-agent-proxy
```

Before the first stable bottle release, use `brew install --HEAD jacobfg/github-agent-proxy/github-agent-proxy` after the packaging changes are merged.

## Binary commands

```sh
github-agent-proxy --version
github-agent-proxy --licenses
github-agent-proxy init-policy                       # JSON to stdout
github-agent-proxy init-policy --output policy.json   # New file, mode 0600
github-agent-proxy init-ca --cert ca.pem --key ca-key.pem
```

These commands need no PAT or running service. `init-policy` emits the embedded default without reading environment settings, contacting GitHub or generating certificates. It refuses to overwrite any existing output path, including a symlink. The default permits reads, authorizes no GitHub writes, uses non-GitHub tunnel mode and contains no destination blocks. Enable owners and repositories explicitly before permitting writes; see the [policy guide](docs/policy.md).

With no subcommand, the binary starts the proxy using the environment below. It defaults to `127.0.0.1:8443`, resolves the PAT identity before listening, and shuts down on SIGINT/SIGTERM. It does not install a service or configure clients.

Sandbox launchers, CLI trust/configuration, local service management and live environment checks belong in the companion checkout's `agent-safehouse-custom/proxy/` directory. The launcher must keep the real PAT and private keys inaccessible to the agent and restrict network access to the proxy. This binary alone does not enforce process isolation. Anyone who can reach an instance shares its configured policy.

## Configuration

| Environment variable | Meaning |
| --- | --- |
| `POLICY_FILE` | JSON policy path; authoritative when set |
| `INTERCEPT_CA_CERT_FILE`, `INTERCEPT_CA_KEY_FILE` | Dedicated signing CA; required in interception mode |
| `GITHUB_TOKEN_FILE` | File containing the PAT; preferred |
| `GITHUB_TOKEN` | Alternative PAT value; mutually exclusive with the file |
| `ALLOWED_ORGS` | Comma-separated organisation logins; case insensitive; empty authorizes no organisation writes |
| `ALLOWED_USERS` | Comma-separated personal-account logins; case insensitive; empty authorizes no personal-repository writes |
| `BRANCH_PREFIX` | Required branch namespace ending in `/`, for example `agent/` |
| `TLS_CERT_FILE`, `TLS_KEY_FILE` | Required PEM certificate and private key |
| `LISTEN_ADDR` | Default `127.0.0.1:8443`; an IP address is required |
| `ALLOW_CONTAINER_BIND` | Set to `1` only to permit binding a non-loopback container address |
| `MAX_PUSH_BYTES` | Maximum receive-pack request size; default 1 GiB |

JSON bodies and GitHub metadata responses are capped at 2 MiB; Git push command headers at 1 MiB. Compressed write requests are rejected. Fetch RPCs may be compressed. Requests have timeouts, and malformed/ambiguous paths or payloads are rejected. Metadata checks and writes are separate GitHub requests, so GitHub offers no atomic guarantee against a concurrent repository transfer or PR head change; the checks intentionally do not cache ownership.

Both owner lists may be empty: reads remain available and all GitHub writes are denied. With `POLICY_FILE`, put owner lists and the branch prefix in JSON and leave their legacy environment variables unset. The process does not load `.env` files.

## Query the running proxy

The local TLS endpoint provides two read-only diagnostics. Use `--noproxy '*'`
to contact the proxy itself, including from a proxy-only sandbox:

```sh
curl --noproxy '*' --fail --silent --show-error https://127.0.0.1:8443/status | jq
curl --noproxy '*' --fail --silent --show-error https://127.0.0.1:8443/healthz
```

Use your configured CA bundle (or `--cacert /path/to/ca-bundle.pem`) if the local
CA is not in the client's default trust store. No Authorization header is needed.

`GET /status` returns the running process's loaded settings, for example:

```json
{
  "allowed_orgs": ["acme"],
  "allowed_users": ["bot"],
  "branch_prefix": "agent/",
  "token_owner": "bot",
  "listen_addr": "127.0.0.1:8443",
  "max_push_bytes": 1073741824,
  "github_reads": "unfiltered",
  "github_writes": "default-deny",
  "non_github_https": "tunnel"
}
```

The allowlists are sorted; empty lists are `[]`. The response excludes the PAT,
TLS private keys, and filesystem paths. These are the proxy's policy settings,
not the PAT's effective GitHub permissions or organisation approvals. Editing an
environment variable or token file does not change the active process; restart
it to load changes. In container mode `listen_addr` describes the container's
listener, rather than its published host port.

`GET /healthz` returns HTTP 200 with `{"status":"ok"}` while the proxy is running,
and HTTP 503 with `{"status":"shutting_down"}` during shutdown. This is a local
liveness check: it does not contact GitHub or verify current PAT access. Startup
still resolves the PAT identity before opening the listener.

Both endpoints support `HEAD`, reject other methods with HTTP 405, and disable
response caching. They apply to local hostnames only; GitHub requests to paths
named `/status` or `/healthz` continue upstream. They share the existing local
listener's access boundary, so anyone able to reach that listener can read them.

## Development and publishing

The [development guide](docs/development.md) covers the offline Go suite, real gh/git tests against local mock servers, policy checks, fuzzing and `Dockerfile.test`. `just build`, `just test` and `just vet` are local shortcuts.

GitHub Actions tests the binary and publishes Apple Silicon Homebrew bottles and Linux container images. The [Homebrew guide](docs/homebrew.md) documents gobottle, the same-repository tap and version tags. Renovate maintains dependencies. The production container image is `ghcr.io/jacobfg/github-agent-proxy:<version>`; deployment belongs to the consumer environment.
