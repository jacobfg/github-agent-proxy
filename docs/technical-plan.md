# Technical Plan

Approved implementation plan. See [brief.md](brief.md) for the problem
statement. Sections marked **as built** record where the implementation
deviated from the plan, and why.

## Architecture

**Stack:** tokio · axum 0.8 · axum-server (rustls) for TLS termination ·
reqwest (rustls, `default-features=false`, **no compression features, redirects
disabled** — bodies pass through byte-identical, PAT can never leak via
redirect) · clap 4 (env) for `serve`/`healthcheck` subcommands · graphql-parser
· moka (owner cache, TTL+singleflight) · flate2 (gzip push bodies) · tracing.
Dev-deps: wiremock, rcgen (per-test certs), nextest.

**Why TLS locally:** `gh` always speaks HTTPS (`GH_HOST=x` →
`https://x/api/v3`). One mkcert cert (`CN=localhost`, trusted locally) serves
both clients on `127.0.0.1:8443`. The loopback leg carries no secrets (the
proxy injects the PAT after receipt), so this is uniformity, not
confidentiality.

**As built — pkt-line codec:** the push-protocol codec is implemented directly
(`policy/git_push.rs`) rather than via gix-packetline. The proxy buffers the
whole push body before parsing, so the streaming edge cases a full library
handles don't apply; the codec is verified byte-for-byte against
git-http-backend's own rejection responses and exercised end-to-end against the
real `git` binary.

**As built — rustls provider:** both aws-lc-rs (rustls default) and ring (via
the rcgen dev-dependency) end up compiled in, so rustls refuses to pick a
process-level `CryptoProvider` automatically. `main()` installs aws-lc-rs
explicitly; without it the binary panics at first TLS use.

### Module layout (`src/`)

```
main.rs            clap entry: serve (default) / healthcheck; installs CryptoProvider
config.rs          GAP_* env config + validation, token-file loading
server.rs          TLS acceptor, router, TraceLayer, graceful shutdown
health.rs          /healthz + healthcheck subcommand
error.rs           ProxyError; JSON renderer (API) vs pkt-line report-status (git push)
upstream.rs        header rewrite + forward + raw response pass-through
proxy/normalize.rs strip /api/v3, map /api/graphql, API-vs-git routing
proxy/classify.rs  method+path → Read | Write(rule) | GitReceivePack | LfsBatch
policy/rules.rs    REST write-rule table
policy/graphql.rs  GraphQL query/mutation enforcement
policy/git_push.rs receive-pack interception (buffer, parse, ref rules, synthetic NG)
owner.rs           startup GET /user identity, author lookups, moka cache
tests/             policy_matrix, owner_check, git_e2e, common harness
```

Every async fn on the request path takes **owned** arguments (`Arc<_>`,
`String`, `Bytes`, `Parts`). A future that captures reference *parameters*
trips rustc's "implementation of `Send` is not general enough" under axum's
`Handler` bound; owned arguments keep the tree region-free.

### Request pipeline

TLS accept → TraceLayer → `/healthz` short-circuit → normalize (`/api/v3`
strip, `/api/graphql` → GraphQL route, `*.git/*` → git upstream) → classify →
for writes: org check → body-field checks (branch/ref prefix) → owner check
(fail closed) → git_push special path for receive-pack → header rewrite (strip
Authorization/Cookie/hop-by-hop; inject the proxy's token, upstream Host, Via) →
forward (streamed; buffered for push) → response pass-through.

**As built — auth differs per upstream.** The REST/GraphQL API accepts
`Authorization: Bearer <PAT>`, but GitHub's git smart-HTTP endpoints reject
Bearer with 401 and require Basic auth with `x-access-token` as the username.
Verified directly against github.com:

| Request | Bearer | Basic `x-access-token:<PAT>` |
|---|---|---|
| `api.github.com/user` | 200 | — |
| `github.com/{o}/{r}.git/info/refs` | **401** | **200** |

`upstream.rs` therefore picks the scheme per upstream (`AuthKind`).

## Write-rule table (v1)

All rules require `{org} ∈ GAP_ALLOWED_ORGS` (case-insensitive). `PREFIX` =
`GAP_BRANCH_PREFIX` (default `agent/`). Unmatched writes → 403 JSON
`{"error":"policy_denied", ...}`.

**REST (api.github.com upstream):**

| Method + path | Decision |
|---|---|
| POST `/repos/{o}/{r}/issues`, issue/PR comments, PR reviews | ALLOW |
| POST `/repos/{o}/{r}/pulls` | ALLOW; body `head` branch must prefix-match (`owner:branch` form: owner must == identity) |
| PATCH/DELETE `.../issues/{n}`, `.../pulls/{n}`, `.../issues/comments/{id}`, `.../pulls/comments/{id}` | OWNER-ONLY (fetch resource, `user.login == identity`, cached) |
| POST/PATCH/DELETE `.../git/refs[...]` | ALLOW; ref must match `refs/heads/<prefix>*` (blocks tag refs; force ok) |
| POST `.../git/blobs\|trees\|commits\|tags` | ALLOW (dangling objects, unreachable without a gated ref write) |
| PUT/DELETE `.../contents/{path}` | ALLOW; body MUST contain explicit `branch` matching prefix (missing → 403, else it writes the default branch) |
| workflow dispatches, releases, merges, labels/assignees/milestones, requested_reviewers, statuses/check-runs, repo admin, hooks/keys/collaborators | DENY v1 |

**GraphQL (`/graphql`) — mandatory layer.** gh's `issue create`, `pr create`,
`comment` and `close` are GraphQL *mutations*, so a REST-only allowlist would
make gh read-only in practice. Queries pass unfiltered; mutations are limited
to an allowlist (`createIssue`, `addComment`, `createPullRequest` with a
`headRefName` prefix check, plus an update/close/delete set gated owner-only via
a cached `node(id)` author lookup). Unparseable documents, subscriptions,
multi-operation documents without `operationName`, and anything not allowlisted
→ 403.

Parsing and planning are **fully synchronous** and the AST never crosses an
`.await`: graphql-parser's types carry a lifetime-bound `Text` bound that fails
rustc's generalization inside an opaque future. `evaluate` plans owned
`MutationCheck`s, drops the document, then performs the lookups.

**As built — mutation id fields vary.** The target object's ID is not always
`input.id`: `CloseIssueInput`/`ReopenIssueInput`/`DeleteIssueInput` use
`issueId`, `UpdatePullRequestInput` uses `pullRequestId`, and the review-comment
inputs use their own names. `owner_id_field()` maps mutation → field; getting
this wrong rejected legitimate `gh issue close` calls.

**Git smart-HTTP (github.com upstream):**

| Request | Decision |
|---|---|
| `info/refs?service=git-upload-pack`, POST `git-upload-pack` (fetch/clone) | ALLOW |
| `info/refs?service=git-receive-pack` | ALLOW (capability advertisement) |
| POST `/{o}/{r}.git/git-receive-pack` (push) | ORG CHECK + every ref update must match `refs/heads/<prefix>*`; create/delete/force ok within prefix; one bad ref rejects the whole push |
| POST `/info/lfs/objects/batch` | ORG CHECK; `operation=download` allow, `upload` DENY v1 |

**Push filtering:** buffer the body up to `GAP_MAX_PUSH_BODY_BYTES` (default
256 MiB → 413 over cap); if gzipped, decompress a *copy* for parsing and forward
the untouched original (byte-identical guaranteed); parse pkt-lines until the
flush `0000` (the first line's `\0capabilities` is ignored). On rejection,
**don't** return a bare HTTP 403 — git hides the body — but synthesize a
receive-pack result so the agent sees
`! [remote rejected] <ref> (policy: ...)`.

**As built — sideband framing.** Under a negotiated sideband the report
stream's terminating flush rides *inside* the band-1 payload, followed by a
single raw flush:

```
<pkt>\x01<pkt>unpack ok\n</pkt><pkt>ng <ref> <reason>\n</pkt>0000</pkt>0000
```

Three wrong variants were tried first; each parsed the report correctly and
*then* failed with "the remote end hung up unexpectedly". The shape above was
derived by diffing packet traces against a real git-http-backend rejection.
`Git-Protocol: version=2` on push → explicit 403 (never misparse). PACK
contents are never inspected; SSH remotes bypass the proxy entirely
(credential starvation is the boundary).

**Owner checks:** resolve the PAT login via `GET /user` at startup (fail
closed; `GAP_OWNER_LOGIN` overrides for offline dev). Per-resource author
fetches are cached in moka (TTL 10 min, 10k entries, singleflight). An upstream
lookup failure → 403, deliberately indistinguishable from a 404 so the proxy
isn't an existence oracle.

## Repo skeleton (adapted from fittools)

- `rust-toolchain.toml` — the single source of truth for the toolchain and its
  components; CI runs `rustup show` to install from it (Renovate's
  `rust-toolchain` manager keeps it current). `dtolnay/rust-toolchain@master`
  was tried first and fails without an explicit `toolchain` input.
- `justfile` — `build test lint fmt verify smoke notices`
- `.github/workflows/test.yml` — a `gates` job (fmt/clippy/nextest/build) plus a
  `container` job matrixed over native runners per arch (build, poll
  healthcheck, smoke script, `if: always()` cleanup)
- `.github/workflows/publish.yml` — semver tag `X.Y.Z` (no `v`): validate tag +
  main ancestry → a native runner per arch pushes by digest → a `merge` job
  combines the digests into `ghcr.io/jacobfg/github-agent-proxy:{ver,latest}`;
  OCI labels, gha cache; perms `contents:read` + `packages:write`
  - **Why native runners, not emulated buildx:** the builder stage runs
    `cargo build` for its *host* target, so building `linux/arm64` on an amd64
    runner produces an amd64 binary published under the arm64 manifest. Proven
    by building the `linux/amd64` image on an arm64 host and inspecting the
    binary (`ELF ... ARM aarch64`). Each build job now executes the image on its
    own native runner, so a mismatch fails the release instead of shipping.
- `.github/workflows/renovatebot.yml` — scheduled self-hosted renovate
  (SHA-pinned action, `RENOVATE_TOKEN`)
- `.github/renovate.json5` — managers `cargo, dockerfile, rust-toolchain,
  custom.regex`; dashboard; no automerge; group minor+patch; major needs
  dashboard approval; custom regex for "Requires Rust" in the README
- `Dockerfile` — `rust:${RUST_VERSION}-alpine` → musl static → `FROM scratch`,
  `USER 10001`, `EXPOSE 8443`,
  `HEALTHCHECK CMD ["/github-agent-proxy","healthcheck","--insecure"]` (~16 MB)

## Test plan

- **Unit:** classifier allow/deny table (every rule ≥1 allow + ≥1 deny); rule
  bodies (`contents` missing branch denied, PR head prefix, tag refs denied);
  graphql (query allowed, non-allowlisted mutation denied, `headRefName`
  enforced, per-mutation id fields, unparseable/multi-op denied); git_push
  parser (create/delete/force/multi-ref-one-bad/garbage/gzip-forwarded-byte-identical);
  owner cache (hit/ttl/fail-closed/singleflight); normalize (`/api/v3` strip,
  GHES preserve mode).
- **Integration (wiremock + rcgen TLS):** PAT injected and client auth stripped;
  read passthrough; `/api/v3` normalized before rules; owner-allowed vs 403 with
  cache-hit proof (mock hit-count 1); graphql create allowed / foreign-update
  403; push forwarded byte-identical / synthetic `ng` for main.
- **git e2e:** the real `git` binary against `git-http-backend` over a temp bare
  repo: clone via insteadOf, push `agent/x` ok, push `main` remote-rejected with
  a `policy:` message, delete + force-push within prefix ok, plus a forwarded
  upstream rejection (regression cover for the synthetic framing). Needs a
  Debian-based image — Alpine's git package omits `git-http-backend`.
- **Smoke (`scripts/smoke.sh`, also CI):** container healthcheck;
  write-without-org → 403 JSON shape; unlisted write → `default_deny`; push
  rejection message contains `policy:`.
- **Live spike (`scripts/spike.sh`):** fully containerized verification against
  real GitHub — proxy and an agent client on a throwaway network, test CA
  generated in a helper container, nothing on the host touched. Covers gh read,
  clone, allowed push, rejected push, GraphQL issue create + owner-only close,
  and a bypass attempt that must 401.

## Risks / known limits (documented in the README)

1. Push bodies are buffered (256 MiB cap) — larger pushes are rejected until a
   streaming parse lands.
2. Protocol-v2 push → explicit 403. 3. LFS upload denied. 4. Redirects are never
   followed (PAT safety). 5. Owner identity is per-PAT, so a shared PAT means a
   shared identity. 6. SSH remotes bypass the proxy. 7. Interactive `gh auth`
   is unsupported. 8. `gh` commands given a **full URL** resolve the host from
   the URL and bypass `GH_HOST` — use the number + `--repo` form. 9. Homebrew
   git needs the scoped `http.<proxy>.sslCAInfo` line (it ignores the macOS
   keychain). 10. Author lookups cost one cached API call per write.
