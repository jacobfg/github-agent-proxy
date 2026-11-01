# github-agent-proxy

A local reverse proxy that gives coding agents **finer-grained GitHub access than GitHub supports**. It sits between the agent's `gh` and `git` CLIs and GitHub, enforcing write policy on every request.

Requires Rust 1.99.0+ (to build from source).

## The model

- **The proxy holds the only real credential** — a fine-grained PAT. It injects `Authorization` into upstream requests and strips whatever the client sent.
- **The agent's environment contains no working GitHub token.** If the agent bypasses the proxy and hits github.com directly, it gets 401s. Bypass *fails closed* — that's the security boundary, not concealment.
- **Reads are never filtered.** Any GET/HEAD, GraphQL query, `git fetch`/`clone`, LFS download.
- **Writes are default-deny**, with a small allowlist:

| Operation | Rule |
|---|---|
| Create issue / comment / PR review | Org allowlist |
| Create pull request | Org allowlist + head branch must match the branch prefix |
| Update/delete issue, PR, comment | Org allowlist + **owner-only** (resource author must be the PAT identity) |
| Create/update/delete git refs | Org allowlist + ref must be under `refs/heads/<prefix>` (tags blocked) |
| Create blobs/trees/commits/tag objects | Org allowlist (dangling objects only) |
| Contents API (create/update/delete file) | Org allowlist + explicit `branch` field matching the prefix |
| `git push` | Org allowlist + every ref under the prefix; create/delete/force-push allowed within it |
| LFS | Download only |
| Workflow dispatches, releases, merges, labels, assignees, reviewers, statuses, repo admin | **Denied** |

Rejected REST/GraphQL writes get a `403` JSON body naming the rule. Rejected pushes come back as a receive-pack result git renders as `! [remote rejected] <ref> (policy: ...)` — the agent can read and act on it.

### Caveats

- **Owner-only means per-PAT, not per-agent-run.** Agents sharing one PAT share an identity.
- **SSH remotes bypass the proxy** (`git@github.com:...`); the credential starvation above is what makes that safe. Optionally also rewrite SSH: `git config --global url."https://github.com/".insteadOf "git@github.com:"`.
- GraphQL comment mutations (update/delete comment) are enforced owner-only; comment nodes expose no repository via the API, so the PAT's own repo scoping is the org backstop there.
- Push bodies are buffered up to `GAP_MAX_PUSH_BODY_BYTES` (default 256 MiB); larger pushes are rejected. PACK contents are never inspected.
- git protocol v2 push is rejected (GitHub doesn't negotiate it by default).
- Interactive `gh auth` flows don't work through the proxy — use the dummy-token env setup below.
- **gh commands given a full URL** (e.g. `gh issue close https://github.com/...`) resolve the host from the URL and bypass `GH_HOST`. Use the number-plus-`--repo` form instead — that's what agent prompts should instruct.
- git smart-HTTP endpoints only accept Basic `x-access-token:<PAT>` upstream (Bearer is API-only) — the proxy handles this translation; just don't be surprised in the logs.

## Setup (one-time)

### 1. Create the PAT

GitHub → Settings → Developer settings → Fine-grained tokens. Restrict to the repositories agents may touch, with permissions: **Contents: read-write**, **Issues: read-write**, **Pull requests: read-write**, plus read-only metadata. Store it at `~/.secrets/gap-pat` (a file, not an env var, so it stays out of process listings).

### 2. Local CA (gh requires TLS)

```sh
brew install mkcert nss
mkcert -install
mkcert -cert-file ~/.config/gap/localhost.pem \
       -key-file  ~/.config/gap/localhost-key.pem \
       localhost 127.0.0.1 ::1
```

### 3. Run the proxy

```sh
github-agent-proxy \
  --token-file ~/.secrets/gap-pat \
  --allowed-orgs myorg1,myorg2 \
  --branch-prefix agent/ \
  --listen 127.0.0.1:8443 \
  --tls-cert ~/.config/gap/localhost.pem --tls-key ~/.config/gap/localhost-key.pem
```

All flags have `GAP_*` env equivalents (`GAP_GITHUB_TOKEN_FILE`, `GAP_ALLOWED_ORGS`, `GAP_BRANCH_PREFIX`, ...). See `--help`.

### 4. Point the agent's tools at it (no per-command changes)

```sh
# git: rewrite github.com URLs at transport time
git config --global url."https://localhost:8443/".insteadOf "https://github.com/"
# Homebrew git/curl ignores the macOS keychain; trust the mkcert CA for the proxy host only
git config --global http."https://localhost:8443/".sslCAInfo "$(mkcert -CAROOT)/rootCA.pem"

# gh: treat the proxy as the (enterprise-style) host; the dummy token is replaced upstream
export GH_HOST=localhost:8443
export GH_ENTERPRISE_TOKEN=dummy
```

The agent then runs completely ordinary commands — `git clone https://github.com/org/repo`, `gh pr create` — with policy applied underneath.

## Apple containers

Build and run with the Apple `container` CLI (macOS 26+):

```sh
container build -t github-agent-proxy .

container run -d --name gap \
  -p 127.0.0.1:8443:8443 \
  -v ~/.secrets/gap-pat:/run/secrets/token:ro \
  -v ~/.config/gap:/certs:ro \
  -e GAP_GITHUB_TOKEN_FILE=/run/secrets/token \
  -e GAP_TLS_CERT=/certs/localhost.pem \
  -e GAP_TLS_KEY=/certs/localhost-key.pem \
  -e GAP_ALLOWED_ORGS=myorg1,myorg2 \
  -e GAP_BRANCH_PREFIX=agent/ \
  ghcr.io/jacobfg/github-agent-proxy:latest
```

Notes:

- The image is `FROM scratch` with a single static binary; the HEALTHCHECK shells the binary's own `healthcheck` subcommand (no shell exists in the image).
- The PAT only ever exists as a read-only mount — never baked into the image, never in the agent's environment.
- **Agent on the host:** use the §4 setup unchanged; the host keychain already trusts mkcert.
- **Agent in another container:** mount `$(mkcert -CAROOT)/rootCA.pem` into it and set `SSL_CERT_FILE=/ca/rootCA.pem` (gh/Go), `GIT_SSL_CAINFO=/ca/rootCA.pem` (git), `GH_HOST=<proxy-container-ip>` (or the published host port), `GH_ENTERPRISE_TOKEN=dummy`, and the same `url.insteadOf` pointing at the proxy address. The agent container never holds a real token.

## GitHub Enterprise

```sh
github-agent-proxy --ghes --upstream-api https://ghes.example.com/api/v3 --upstream-git https://ghes.example.com ...
```

`--ghes` preserves the `/api/v3` prefix when forwarding (a real GHES expects it) and routes GraphQL to `/api/graphql` on the same host.

## Development

```sh
just            # fmt + clippy + nextest + build (same gates as CI)
just smoke      # policy smoke test against a locally running proxy
just notices    # regenerate THIRD_PARTY_NOTICES.md (needs cargo-about)
```

Live end-to-end verification against real GitHub (fully containerized — proxy
and an "agent" client on a throwaway docker network; nothing on the host is
touched):

```sh
GAP_SPIKE_REPO=owner/empty-test-repo ./scripts/spike.sh
```

CI (GitHub Actions): `just verify` on every push/PR, then a multi-arch buildx build and a container smoke test. Tag `X.Y.Z` (no `v` prefix) publishes multi-arch images to GHCR. Renovate runs self-hosted on a schedule (needs the `RENOVATE_TOKEN` repo secret).

## Design docs

- [docs/brief.md](docs/brief.md) — problem statement and locked decisions
- [docs/technical-plan.md](docs/technical-plan.md) — architecture, full write-rule table, risks
