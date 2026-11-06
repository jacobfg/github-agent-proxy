# Development and tests

Use the Go version in `go.mod` and run these commands from the repository root. The local `justfile` offers `build`, `test`, `vet`, `fmt` and `check-fmt`; CI runs its commands directly. It does not load `.env`, configure credentials or start services. Tests never contact GitHub and do not need a real token. Install both `git` and `gh` to run the CLI integration tests; they explicitly skip if either binary is absent, and CI requires both.

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
| `TestInitPolicy`, `TestDefaultPolicyReadOnly` | Credential-free policy generation, exclusive file creation, read-only defaults through the real parser and REST/GraphQL/Git authorization |
| `TestInstallSkill`, `TestInstallSkillRejectsExistingPaths`, `TestInstallSkillArguments` | Embedded skill display/installation without service configuration, matching stdout and installed content, preservation of existing destinations and symlinks, argument validation and help |
| `TestConfiguration`, `TestPolicyFileFailures`, `TestRunRequiresPolicyFile` | Mandatory policy, malformed/missing files, invalid branch prefix, required credentials/TLS, loopback binding, invalid push limits |
| `TestServiceFlagPrecedence`, `TestServiceFlagsValidation`, `TestServiceMixedConfiguration`, `TestHelpDoesNotRequireServiceConfiguration` | Flags override environment before validation, explicit empty/false overrides, mixed inputs, credential conflicts, invalid arguments, credential-free help |
| `TestOwnerAllowlistConfiguration` | Organisations only, users only, both, neither, read-only empty lists, login normalization |
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

`TestConfiguredPolicy` requires `POLICY_FILE`, pointing to a valid JSON policy. Supply `POLICY_CHECK_REPO=OWNER/REPO` and `POLICY_CHECK_OWNER_TYPE=User` or `Organization` to describe the repository being simulated. The owner type is explicit so the test can detect a personal username placed in the organisation list, and vice versa.

Generate a read-only policy, or use an existing operator-owned policy:

```sh
github-agent-proxy init-policy --output policy.json
POLICY_CHECK=1 \
POLICY_CHECK_REPO=jacobfg/permission-test \
POLICY_CHECK_OWNER_TYPE=User \
POLICY_FILE="$PWD/policy.json" \
go test -v -count=1 ./internal/proxy -run '^TestConfiguredPolicy$'
```

Or build and run the test image, mounting only the policy:

```sh
docker build -f Dockerfile.test -t github-agent-proxy:test .
docker run --rm --network none \
  --mount "type=bind,source=$PWD/policy.json,target=/run/policy.json,readonly" \
  -e POLICY_FILE=/run/policy.json \
  -e POLICY_CHECK=1 \
  -e POLICY_CHECK_REPO=jacobfg/permission-test \
  -e POLICY_CHECK_OWNER_TYPE=User \
  github-agent-proxy:test \
  go test -v -count=1 ./internal/proxy -run '^TestConfiguredPolicy$'
```

The generated policy blocks all writes. To exercise allowed writes, edit `github.allowed_users` to include `jacobfg` and `github.write_allow_repos` to include `jacobfg/permission-test`. To check a forbidden owner, change the user list to another account. For an organisation, set `POLICY_CHECK_OWNER_TYPE=Organization` and configure `github.allowed_orgs` instead. The `github.branch_prefix` must be a valid namespace ending in `/`. Empty owner lists are valid and deny all writes.

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
