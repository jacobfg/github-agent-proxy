# GitHub agent proxy

A Go proxy that keeps a fine-grained GitHub PAT outside a coding agent's environment and applies a narrower write policy. Normal `gh` and `git` commands use a local TLS endpoint. For GitHub, client credentials are discarded and the proxy injects its own token only into requests to fixed GitHub upstreams. With a JSON policy, the proxy can intercept all HTTPS, enforce TLS 1.2+, block destinations by hostname/IP/CIDR, and narrow GitHub writes by repository. Legacy configuration keeps opaque non-GitHub CONNECT tunnels. See the [policy examples and isolated trial guide](docs/policy.md).

GitHub reads have no repository-owner, author, or branch restrictions. GitHub still applies the PAT's own permissions. GitHub writes are denied unless explicitly supported below.

## Policy

An allowed repository owner is an organisation listed in `ALLOWED_ORGS` or a personal account listed in `ALLOWED_USERS`. Either list may be unset or empty, and both may be set together. At least one must contain a login. GitHub's owner type must match the list: putting a username in `ALLOWED_ORGS` does not allow its personal repositories.

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

The repository also contains a source-based Homebrew formula. Native CI builds and tests real bottles for Intel and Apple Silicon macOS. The package contains the binary, docs and an example policy; your trusted sandbox launcher owns proxy startup, configuration and cleanup. See [Homebrew installation and releases](docs/homebrew.md).

```sh
brew tap jacobfg/github-agent-proxy https://github.com/jacobfg/github-agent-proxy
brew trust --formula jacobfg/github-agent-proxy/github-agent-proxy # Skip on older Homebrew without trust.
brew install jacobfg/github-agent-proxy/github-agent-proxy
```

Before the first stable bottle release, use `brew install --HEAD jacobfg/github-agent-proxy/github-agent-proxy` after the packaging changes are merged.

## Start with Apple Container

The [justfile](justfile) provides local development shortcuts. GitHub Actions continues to run its commands directly. Install `just` with `brew install just`, copy `.env.example` to `.env`, and set `ALLOWED_USERS=jacobfg` for personal-repository testing, `ALLOWED_ORGS` for organisations, or both. The file contains policy settings only; keep the PAT in the separate private directory.

```sh
just                     # List recipes
just system-start        # Start Apple Container
just certs               # Install mkcert CA and generate certificates
# Put your PAT in ~/.config/github-agent-proxy-private/token with a trusted editor.
just run                 # Build and start the proxy on host loopback
just logs                # Follow proxy logs
just stop                # Stop the proxy
just remove              # Delete the stopped container before recreating it

just test                # Build the test image and run the full offline suite
just test-shell          # Open a disposable shell with Go, gh, Git, and the proxy
just test-live           # Start the proxy and a configured gh/git shell using your test PAT
just policy jacobfg/permission-test User
just policy your-org/repo Organization
```

`test`, `test-shell`, and `policy` use no real credentials. `test-live` mounts the private directory, configures Git TLS trust and the shared HTTPS proxy, checks proxy readiness, and opens an interactive shell. Exiting that shell stops the temporary proxy and removes the container. Its commands contact real GitHub; use your disposable test repository. This combined container is a development environment with access to the mounted PAT.

Recipes build their images before running them so source changes take effect. To rebuild without starting anything, use `just build` or `just test-build`. To apply new settings to the named proxy, use `just stop`, `just remove`, then `just run`. `just certs` regenerates the certificate pair, so restart the proxy after using it. Shell environment variables override `.env`; the optional settings in `.env.example` include the private directory, image names, container name, and host port. A custom host port also requires updating the host CLI proxy URL.

Requires Apple silicon, a supported macOS version for [Apple Container](https://github.com/apple/container), `mkcert`, `gh`, and Git. Install the tools and start the container service from a trusted operator shell:

```sh
brew install container mkcert gh git
mkcert -install
container system start
container build -t github-agent-proxy:local .
```

Create a fine-grained PAT with the intended organisation or personal account as resource owner and only the repositories the agent needs. Grant Contents, Issues, and Pull requests read/write, plus Metadata read. Grant additional read permissions only where needed. Organisation approval or SSO may be required. A fine-grained PAT has a single resource owner: listing several owners does not expand the token's access.

Store the PAT and TLS private key outside the agent's filesystem access. These examples use an operator-owned directory; the agent sandbox must not be able to read it or control the container runtime.

```sh
PROXY_PRIVATE="$HOME/.config/github-agent-proxy-private"
mkdir -p "$PROXY_PRIVATE"
chmod 700 "$PROXY_PRIVATE"
mkcert -cert-file "$PROXY_PRIVATE/server.pem" \
  -key-file "$PROXY_PRIVATE/server-key.pem" \
  localhost 127.0.0.1 ::1 api.github.com github.com

# Paste the PAT into this file with a trusted editor. Do not put it in shell history.
touch "$PROXY_PRIVATE/token"
chmod 600 "$PROXY_PRIVATE/token" "$PROXY_PRIVATE/server-key.pem"
"${EDITOR:-vi}" "$PROXY_PRIVATE/token"

container run --detach --name github-agent-proxy \
  --publish 127.0.0.1:8443:8443 \
  --user "$(id -u):$(id -g)" \
  --read-only \
  --mount "type=bind,source=$PROXY_PRIVATE,target=/run/proxy,readonly" \
  --env GITHUB_TOKEN_FILE=/run/proxy/token \
  --env ALLOWED_ORGS=your-org \
  --env BRANCH_PREFIX=agent/ \
  --env LISTEN_ADDR=0.0.0.0:8443 \
  --env ALLOW_CONTAINER_BIND=1 \
  --env TLS_CERT_FILE=/run/proxy/server.pem \
  --env TLS_KEY_FILE=/run/proxy/server-key.pem \
  github-agent-proxy:local

container logs github-agent-proxy
```

For a personal test repository such as `jacobfg/permission-test`, replace `--env ALLOWED_ORGS=your-org` with `--env ALLOWED_USERS=jacobfg`. To allow both account types, supply both environment variables. After changing the configuration, restart the proxy process; after updating the code, rebuild the image and recreate the container.

The service defaults to `127.0.0.1:8443`. Port publishing requires it to listen on the container interface, so the example explicitly enables that exception inside the isolated VM and publishes **only** on host loopback. Do not use this override for a native host process. Apple Container may also expose the VM address to the host and other containers: keep untrusted workloads off the proxy's network. All clients that reach this endpoint share the configured policy; it has no separate client authentication.

The image runs without root, a shell, or a writable root filesystem. The user override lets it read host-owned `0600` bind-mounted files. Stop it with `container stop github-agent-proxy`. After publication, the equivalent image name is `ghcr.io/jacobfg/github-agent-proxy:<version>`.

## Host networking for VPN and corporate services

Apple Container does not necessarily inherit the Mac's VPN or per-application
network access. A hostname can resolve inside the VM while TCP connections time
out. In that case non-GitHub CONNECT requests return 502. For example, Claude's
`apiKeyHelper` may report only `authentication failed` when it cannot contact its
gateway through the container, even though its saved credentials are valid.

Run the same proxy natively from a trusted operator shell to use the Mac's network:

```sh
just stop         # Release port 8443 if the Apple Container proxy is running
just host-start   # Build and launch the native proxy; also restarts it after edits
just host-logs    # Check startup; Ctrl-C stops following logs, not the proxy
just host-stop   # Stop the native proxy
```

This requires Go, Python 3, and a logged-in macOS GUI session. `host-start` installs
the executable, launchd configuration, and logs in `PROXY_PRIVATE`, alongside the
existing token file and TLS keys. The launchd configuration contains the token's
file path, never the token itself. Keep the whole directory inaccessible to the
agent; do not run a privileged proxy from an agent-writable build directory.
The proxy still binds only to `127.0.0.1`, with the same TLS and GitHub policy.

The launchd job survives terminal closure and restarts after crashes. Run
`just host-start` again after logout/reboot; no login-startup entry is installed.
Only one proxy can own the port. To return to Apple Container, run `just host-stop`
then `container start github-agent-proxy` (or recreate it with `just run`).
The sandbox proxy settings and strict network profile work with either mode.
Do not add a gateway to `NO_PROXY` to work around a container routing failure.

## One-time agent CLI setup

Keep the agent's GitHub identity as `github.com`. Git and `gh` use the shared `HTTPS_PROXY` environment variable; the explicit Git setting below also works. Rewrite SSH URLs to GitHub HTTPS URLs. For these two GitHub destinations, the proxy terminates TLS, applies the policy, and opens a separate verified TLS connection to GitHub. This preserves repository detection for commands such as `gh repo view` without an explicit repository argument.

In the trusted shell, copy **only the public CA certificate** into a directory visible to the agent:

```sh
AGENT_CONFIG="$HOME/.config/github-agent-cli"
mkdir -p "$AGENT_CONFIG"
cp "$(mkcert -CAROOT)/rootCA.pem" "$AGENT_CONFIG/proxy-ca.pem"
# macOS system roots; on Debian/Alpine use /etc/ssl/certs/ca-certificates.crt.
cat /etc/ssl/cert.pem "$AGENT_CONFIG/proxy-ca.pem" > "$AGENT_CONFIG/ca-bundle.pem"
# Include your corporate public CA bundle too, if required by your network.
```

Set these values in the agent's shell/profile. The certificate path must exist in the agent environment. On macOS, `mkcert -install` also establishes system trust; `SSL_CERT_FILE` supplies the CA for Go clients on Linux.

```sh
export GH_TOKEN=proxy-placeholder-not-a-real-token
export GITHUB_TOKEN="$GH_TOKEN"
export GH_CONFIG_DIR="$HOME/.config/github-agent-cli/gh"
export GH_PROMPT_DISABLED=1
export HTTPS_PROXY=https://127.0.0.1:8443
export https_proxy="$HTTPS_PROXY"
export HTTP_PROXY="$HTTPS_PROXY" http_proxy="$HTTPS_PROXY"
export ALL_PROXY="$HTTPS_PROXY" all_proxy="$HTTPS_PROXY"
export NO_PROXY= no_proxy=
export SSL_CERT_FILE="$HOME/.config/github-agent-cli/ca-bundle.pem"
export GIT_SSL_CAINFO="$SSL_CERT_FILE"
export CURL_HOME="$HOME/.config/github-agent-cli/curl"
mkdir -p "$CURL_HOME"
printf 'proxy-cacert = "%s"\n' "$SSL_CERT_FILE" > "$CURL_HOME/.curlrc"
unset GH_HOST GH_ENTERPRISE_TOKEN GITHUB_ENTERPRISE_TOKEN SSH_AUTH_SOCK

# Run these in the agent's own Git configuration, not a shared operator profile.
git config --global http."https://github.com/".proxy "$HTTPS_PROXY"
git config --global --add url."https://github.com/".insteadOf git@github.com:
git config --global --add url."https://github.com/".insteadOf ssh://git@github.com/
git config --global http."https://github.com/".sslCAInfo "$SSL_CERT_FILE"
git config --global http.proxySSLCAInfo "$SSL_CERT_FILE"
git config --global credential."https://github.com".helper ''
```

If you used the earlier localhost URL rewrites, remove their `url` sections from the agent's Git config (for example `git config --global --remove-section 'url.https://127.0.0.1:8443/git/'`). Keep remote URLs on `github.com`: `gh` inspects Git's effective remote URLs and cannot infer a GitHub repository from a localhost URL. The proxy's `/git/` route remains available for direct transport requests, but should not be used as a repository's effective remote when using `gh`.

Use a fresh agent home/config with no real tokens, credential helpers containing real credentials, SSH keys, browser sessions, or access to the operator's keychain. A placeholder environment variable alone does not remove stored credentials. Keep the PAT, TLS private key, mkcert CA private key, container control socket, and proxy configuration inaccessible to the agent. The proxy cannot provide this isolation for a process with the operator's filesystem privileges.

The CONNECT endpoint intercepts `api.github.com:443` and `github.com:443`, including case and trailing-dot variants. Those hosts on other ports are denied. Every other destination is connected directly by the proxy, which resolves its hostname and relays bytes without reading or modifying its TLS. The PAT and outer proxy authentication headers are never added to those tunnels. This includes GitHub asset/archive redirects and non-GitHub services. GitHub Enterprise uses ordinary forwarding and has no PAT injection or write policy.

Non-GitHub CONNECT targets may use any TCP port, including HTTPS services on ports such as 6443. CONNECT is a byte tunnel, not an application-layer HTTPS validator: a client can carry other TCP protocols through it. UDP/QUIC and ordinary HTTP forward requests are not supported. The proxy does not inherit upstream proxy environment variables. Private/VPN destinations must be reachable from the proxy's own host/container network.

Use a CA bundle containing the public mkcert root **and** the usual system/corporate roots. Trusting only mkcert is insufficient for other end-to-end HTTPS destinations. Some clients need their own proxy configuration; programs that ignore the environment fail closed under the network policy below.

The examples assume the CLIs run on the Mac host or share its network namespace. Inside a separate agent VM/container, `127.0.0.1` names that VM: arrange a trusted loopback tunnel to the host proxy instead of publishing it on the LAN.

Now normal commands work:

```sh
gh api user --jq .login
gh issue list --repo your-org/repo
git clone https://github.com/your-org/repo.git
cd repo
git switch -c agent/example
# Make a change and commit it, then:
git push -u origin agent/example
gh pr create --base main --head agent/example --title 'Example' --body 'Description'
```

Check the boundary without making a write:

```sh
# Resolves the proxy's PAT identity:
gh api user --jq .login

# Bypasses the proxy and sends only the invalid placeholder: expect HTTP 401.
curl --noproxy '*' --silent --output /dev/null --write-out '%{http_code}\n' \
  -H "Authorization: Bearer $GH_TOKEN" https://api.github.com/user
```

Without the optional network restriction below, public reads may still work directly without credentials. Authenticated writes cannot bypass the proxy unless the agent obtains another credential. GitHub repository rules, PAT permissions, and server-side hooks remain in force even when the proxy allows a request. Branch writes can trigger repository automation: configure that automation's permissions and secrets for the agent branch namespace accordingly.

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

## Enforce proxy-only networking with Safehouse

Proxy environment variables route cooperative clients. A Seatbelt profile enforces the boundary even when a process clears them. Append this **after** the other Safehouse profiles:

```scheme
(deny network*)
(deny network-outbound (require-not (remote tcp4 "localhost:8443")))
(deny network-bind network-inbound (local ip) (local unix-socket))
(deny system-socket)
(deny mach-lookup (global-name "com.apple.dnssd.service"))
(allow network-outbound (remote tcp4 "localhost:8443"))
```

Seatbelt requires `localhost` in this expression; `tcp4` limits the exception to IPv4 loopback. Configure clients with `https://127.0.0.1:8443` and empty `NO_PROXY`/`no_proxy`. The final policy denies direct TCP, UDP (including UDP to port 8443), IPv6, UNIX socket connections, and local listening sockets. Run the proxy outside this sandbox. Existing shells must be restarted to receive a new policy.

Remove Docker/Colima permissions and other host execution/relay services. This is a socket restriction, not a complete audit of macOS IPC: any separately permitted host service capable of making requests on the agent's behalf must also be constrained. Do not pass pre-opened network sockets into the sandbox or let untrusted code change the launcher/profile. Destination DNS resolution for CONNECT happens in the proxy.

The companion `agent-safehouse-custom/run-in-sandbox` installs shared proxy variables and this final profile; it uses the real `gh` directly, without a wrapper. Tools that need to listen locally (dev servers, OAuth callbacks), use raw UDP, or connect directly will fail under this strict profile.

## Configuration

| Environment variable | Meaning |
| --- | --- |
| `GITHUB_TOKEN_FILE` | File containing the PAT; preferred |
| `GITHUB_TOKEN` | Alternative PAT value; mutually exclusive with the file |
| `ALLOWED_ORGS` | Comma-separated organisation logins; case insensitive; may be unset or empty if `ALLOWED_USERS` is non-empty |
| `ALLOWED_USERS` | Comma-separated personal-account logins; case insensitive; may be unset or empty if `ALLOWED_ORGS` is non-empty |
| `BRANCH_PREFIX` | Required branch namespace ending in `/`, for example `agent/` |
| `TLS_CERT_FILE`, `TLS_KEY_FILE` | Required PEM certificate and private key |
| `LISTEN_ADDR` | Default `127.0.0.1:8443`; an IP address is required |
| `ALLOW_CONTAINER_BIND` | Set to `1` only to permit binding a non-loopback container address |
| `MAX_PUSH_BYTES` | Maximum receive-pack request size; default 1 GiB |

JSON bodies and GitHub metadata responses are capped at 2 MiB; Git push command headers at 1 MiB. Compressed write requests are rejected. Fetch RPCs may be compressed. Requests have timeouts, and malformed/ambiguous paths or payloads are rejected. Metadata checks and writes are separate GitHub requests, so GitHub offers no atomic guarantee against a concurrent repository transfer or PR head change; the checks intentionally do not cache ownership.

To run natively, set the required variables in a trusted environment and use the default loopback binding:

```sh
go build -o bin/github-agent-proxy ./cmd/github-agent-proxy
./bin/github-agent-proxy
```

## Development and automation

Use the Go version in `go.mod`. Tests never contact GitHub and do not need a real token. Install both `git` and `gh` to run the CLI integration tests; they explicitly skip if either binary is absent, and CI requires both.

```sh
go test -race -count=1 -timeout=3m ./...
go vet ./...
go test ./internal/proxy -run '^$' -fuzz FuzzPushCommands -fuzztime 10s
```

Tests cover write allow/deny decisions, authorship, GraphQL aliases/fragments/defaults, malformed payloads, credential replacement, TLS interception, and real CLI traffic. Git integration tests use a temporary local `git http-backend` and verify that forbidden or mixed pushes leave protected refs unchanged.

### Test inventory

The regular suite uses fixed fixtures; exporting your allowlists does not change those fixtures. No test calls live GitHub. API metadata and write responses are mocked; CLI integration tests execute real binaries and use a real local Git backend. Forwarding tests use local TLS servers with independent certificates; they never call external destinations.

| Test | Coverage |
| --- | --- |
| `TestLocalStatus`, `TestLocalHealthAndMethods`, `TestDiagnosticsDoNotShadowGitHub` | Active policy without secrets, local health/shutdown, method restrictions, no upstream calls, GitHub routing preserved |
| `TestConfiguration` | Required credentials/TLS/prefix, default settings, loopback binding, invalid push limits |
| `TestOwnerAllowlistConfiguration` | Organisations only, users only, both, neither, empty entries, login normalization |
| `TestOwnerAllowlists` | REST, GraphQL, and pushes under each owner-list combination; wrong account types; author and branch restrictions |
| `TestRESTPolicy` | Issue/PR creation and edits, comments, refs, contents, branch rename; forbidden operations, duplicate JSON, query overrides, ambiguous paths |
| `TestGraphQLPolicy` | Queries/mutations, authorship, PR heads, refs, aliases, defaults, fragments/cycles, mixed mutations, directives, duplicate inputs, operation selection, missing metadata |
| `TestPushCommands` | Create/force/delete, shallow boundaries and protected refs, capability whitespace, tags, mixed refs, near-prefix names, unsupported capabilities, malformed/truncated packets, duplicate refs |
| `TestGitCLI` | Real clone/push/force/delete for both owner types, shallow-clone branch creation, protected-ref checks, HTTPS proxy certificate trust |
| `TestGHCLI` | Real implicit `gh repo view` and `gh issue create`, HTTPS/SSH remote detection, REST and GraphQL commands for both owner types, accepted and rejected writes |
| `TestHTTPSConnect` | Verified TLS interception, API/Git hosts, requests inside CONNECT, rejection of alternate GitHub ports |
| `TestConnect*` | End-to-end HTTPS/HTTP2 streaming and credentials, original origin certificate, no proxy-header leakage, target validation, normalized GitHub interception, dial failure, buffered tunnel bytes, shutdown |
| `TestHostAndEncodingGuards` | Unsafe hosts/URLs and compressed write rejection |
| `TestReadRPCs` | Fetch RPC and LFS downloads allowed, LFS uploads denied |
| `TestMetadataFailureDeniesWrites` | GraphQL errors, missing nodes, and invalid JSON deny writes before forwarding |
| `TestStartupIdentityAndRedirectSafety` | PAT identity lookup, upstream credential injection, environment-proxy bypass, authenticated redirects not followed |
| `FuzzPushCommands` | Arbitrary push-packet input, parser stability, replay-prefix invariants; extended fuzzing is opt-in |
| `TestConfiguredPolicy` | Opt-in offline report for your allowlists, repository owner type, and branch prefix |

List test names with `go test ./internal/proxy -list .`. Run with `-v` to see named subtests. Fuzz seeds run with the normal suite; use `-fuzz FuzzPushCommands -fuzztime 10s` to generate additional inputs.

### Check your own allowlists offline

`TestConfiguredPolicy` accepts `ALLOWED_ORGS`, `ALLOWED_USERS`, and `BRANCH_PREFIX`. Supply `POLICY_CHECK_REPO=OWNER/REPO` and `POLICY_CHECK_OWNER_TYPE=User` or `Organization` to describe the repository being simulated. The owner type is explicit so the test can detect a personal username placed in the organisation list, and vice versa.

```sh
POLICY_CHECK=1 \
POLICY_CHECK_REPO=jacobfg/permission-test \
POLICY_CHECK_OWNER_TYPE=User \
ALLOWED_ORGS= ALLOWED_USERS=jacobfg BRANCH_PREFIX=agent/ \
go test -v -count=1 ./internal/proxy -run '^TestConfiguredPolicy$'
```

Or rebuild and run the test image:

```sh
docker build -f Dockerfile.test -t github-agent-proxy:test .
docker run --rm --network none \
  -e POLICY_CHECK=1 \
  -e POLICY_CHECK_REPO=jacobfg/permission-test \
  -e POLICY_CHECK_OWNER_TYPE=User \
  -e ALLOWED_ORGS= \
  -e ALLOWED_USERS=jacobfg \
  -e BRANCH_PREFIX=agent/ \
  github-agent-proxy:test \
  go test -v -count=1 ./internal/proxy -run '^TestConfiguredPolicy$'
```

Both allowlists may be set together. To check a forbidden owner, keep the target repository the same and set `ALLOWED_USERS=someone-else`; reads should remain allowed and all writes should be blocked. To check an organisation, set `POLICY_CHECK_OWNER_TYPE=Organization` and configure `ALLOWED_ORGS` accordingly. Unset or empty `BRANCH_PREFIX`, or two empty owner lists, fail configuration validation.

The report covers 40 scenarios: reads, own/other-author issue and PR operations, allowed/forbidden PR heads, branch refs and contents, renames, repository deletion, Git fetch/push/force/delete/mixed refs/tags, and LFS download/upload. Example verdicts:

```text
ALLOW | create issue REST | HTTP 200
BLOCK | edit another author's issue | HTTP 403
ALLOW | Git push allowed branch | HTTP 200
BLOCK | Git push forbidden branch | HTTP 403
```

`PASS` means every decision matched the expected policy, including expected blocks. The test also checks that blocked writes never reach the mock upstream. This report uses a fake PAT identity (`bot`), mock authors (`bot` or `other`), synthetic Git requests, and simulated GitHub responses. It does not run live CLI writes, establish that the repository exists or has the supplied owner type, or validate PAT permissions or GitHub rules. Tokens and token files from your environment are ignored.

### Isolated test container

`Dockerfile.test` includes the compiled `github-agent-proxy` on `PATH`, the Go toolchain and source, `gh`, Git (including `http-backend`), Bash, curl, and OpenSSL. It runs the full race-enabled test suite by default, including both CLI integration tests. Dependencies are downloaded during the build; running the tests requires no GitHub credentials, host mounts, published ports, or external network access.

```sh
docker build -f Dockerfile.test -t github-agent-proxy:test .
docker run --rm --network none github-agent-proxy:test

# Open a disposable shell, or run a specific integration test:
docker run --rm -it --network none github-agent-proxy:test bash
docker run --rm --network none github-agent-proxy:test \
  go test -race -v -count=1 ./internal/proxy -run 'Test(GitCLI|GHCLI)$'
```

With Apple Container:

```sh
container build -f Dockerfile.test -t github-agent-proxy:test .
container run --rm github-agent-proxy:test
container run --rm -it github-agent-proxy:test bash
```

The tests start their own local proxy and mock GitHub servers with temporary TLS certificates. The standalone `github-agent-proxy` binary is also available for manual testing; it needs the configuration described above and network access to GitHub. This combined test image is a development environment, not the credential-isolation boundary used by the production image.

### Certificate trust for live container tests

Copy the public root CA from the same mkcert installation that issued `server.pem` into the mounted private directory:

```sh
# On the host, before starting the test container:
PROXY_PRIVATE="$HOME/.config/github-agent-proxy-private"
cp "$(mkcert -CAROOT)/rootCA.pem" "$PROXY_PRIVATE/rootCA.pem"
```

Inside the container, with that directory mounted at `/run/proxy`, configure both Go and Git's TLS trust:

```sh
export SSL_CERT_FILE=/run/proxy/rootCA.pem
export GIT_SSL_CAINFO="$SSL_CERT_FILE"
git config --global http.proxySSLCAInfo "$SSL_CERT_FILE"
openssl verify -CAfile "$SSL_CERT_FILE" /run/proxy/server.pem
```

The verification must report `server.pem: OK`. If it fails, the CA does not validate the mounted server certificate: copy the matching public CA, or regenerate the server certificate using the intended mkcert installation. Restart the proxy if you replace its certificate.

`SSL_CERT_FILE` supplies trust for `gh`'s Go HTTP client. Git needs `GIT_SSL_CAINFO` for the destination and `http.proxySSLCAInfo` for the separate TLS connection to an HTTPS proxy. `gh repo clone` invokes Git and needs those Git settings too. Omitting the proxy CA can produce `SSL certificate OpenSSL verify result: unable to get local issuer certificate (20)` even when the destination CA is configured correctly. Keep certificate verification enabled.

Automation

- Push/PR CI runs tests with the race detector, `go vet`, formatting checks, and an amd64/arm64 image build.
- A stable version tag such as `0.1.0` must point to a commit on `main`. The publish job reruns tests, then publishes version and `latest` tags to GHCR.
- Scheduled/manual Renovate maintains Go modules, the build image, and GitHub Actions. Set the repository secret `RENOVATE_TOKEN` with repository contents, pull requests, issues, and workflow permissions appropriate to dependency PRs. Updates require review; automerge is off.

No live GitHub writes are part of the test suite. Test any newly allowed mutation against a disposable repository before broadening a production policy.

### JSON policy and HTTPS interception trial

See [the policy guide](docs/policy.md) for hostname/IP examples, repository allow/deny precedence, JSON migration, CA trust, `just trial-start`, the separate Safehouse launcher, and rollback. The trial uses port 8444 and leaves the existing service on 8443 running.
