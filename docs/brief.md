# Brief: github-agent-proxy

## Goal

Finer-grained GitHub access for coding agents than GitHub natively supports.

## Approach

A reverse proxy, written in Rust with a required test suite, used by both the
`gh` and `git` CLIs.

## Requirements

- **Allow all read functions** — no filtering.
- **Any write** (delete, update, create PR, issue, branch, etc.) must be limited to:
  - GitHub organisations passed via an env var.
  - A specified branch prefix, passed via an env var.
  - Issues/PRs: where possible, updates/deletes limited to resources created by
    the token owner.
- Repo automation skeleton follows `jacobfg/fittools`: GitHub Actions for test
  and publishing, automated dependency updates via Renovate.
- Usage guide for starting the container via Apple containers, and settings for
  the gh/git CLIs.
- Bind to 127.0.0.1 (or a file socket) for safety.

## Key design question (resolved)

**Auth in the reverse proxy or provided by the client?** → In the reverse proxy.
The proxy holds the real credentials; the agent environment has none, so
bypassing the proxy fails closed (direct github.com access returns 401).

## Decisions (locked)

| Question | Decision |
|---|---|
| Auth | Proxy-held fine-grained PAT; proxy injects `Authorization`, strips the client's |
| Interception | Seamless URL rewrite — one-time setup; agent types normal commands |
| Issue/PR edits | Owner-only (author == PAT identity) |
| git push | Branch-prefix only; deletes and force-push allowed within prefix |
| Reads | Unfiltered |
| Writes | Default-deny with explicit allowlist |
| Binding | 127.0.0.1 + TLS (gh/git can't use unix sockets) |

See [technical-plan.md](technical-plan.md) for the design and
[../README.md](../README.md) for usage.
