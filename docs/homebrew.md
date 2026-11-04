# Homebrew bottles

This repository is both the source repository and a Homebrew tap. The formula builds the Go program from source; CI turns that installation into real Homebrew bottles for Intel and Apple Silicon macOS 15 or newer. Bottles and their source archive are hosted as GitHub Release assets in this repository.

```sh
brew tap jacobfg/github-agent-proxy https://github.com/jacobfg/github-agent-proxy
brew install jacobfg/github-agent-proxy/github-agent-proxy
github-agent-proxy --version
```

Until the first stable release has been published, only `brew install --HEAD jacobfg/github-agent-proxy/github-agent-proxy` is available. Once bottles exist, normal installs use the matching bottle. Unsupported bottle platforms can build from source where the formula's macOS requirement is met.

## What is installed

The package contains the proxy binary, documentation, third-party notices, and an example policy under `$(brew --prefix github-agent-proxy)/share/github-agent-proxy/policy.example.json`. Go is a build-only dependency. Git, gh and the race-test toolchain are used in CI, not required by the installed proxy. Certificate generation can use the existing mkcert setup or the binary's `init-ca` command as appropriate to the TLS mode.

The example uses opaque non-GitHub tunnelling and an empty repository write allowlist, so it authorizes no repository writes. Replace `CHANGE-ME` and choose the owner/repository rules before using it. The installer never copies it over an active policy, creates credentials, installs CA trust, or launches a process.

## Per-agent process ownership

There is no Homebrew service or launchd definition. A trusted agent launcher should start the proxy outside the agent sandbox, supply `POLICY_FILE`, `GITHUB_TOKEN_FILE`, `TLS_CERT_FILE` and `TLS_KEY_FILE`, and select a separate loopback port for each concurrent instance. Interception mode additionally needs the interception CA paths.

The launcher must wait for readiness, point the agent's CLI proxy/trust configuration at that endpoint, restrict the sandbox to that instance's port, forward termination signals and clean up the child proxy when the agent exits. Policies, PAT files and signing keys must remain outside the sandbox's readable/writable paths. Multiple instances can share operator-controlled CA and credential files; they do not require separate trust roots.

The existing companion `run-in-sandbox` still selects the shared 8443 service (or the opt-in 8444 trial). Installing this package does not change that launcher or automatically implement per-agent process supervision. The package deliberately supplies only the binary and examples so the launcher can own that lifecycle.

## Build and release

The Homebrew workflow builds on `macos-15` (Apple Silicon) and `macos-15-intel`. Pull requests run the same offline Go race suite, vet, Python tooling tests, formula build, `brew test`, bottle creation and bottle reinstall checks as releases. They upload workflow artifacts but cannot publish a release or update the tap.

After merging to `main`, push a stable version tag such as `0.1.0` (without a `v` prefix). The release workflow verifies that the tag belongs to `main`, builds a deterministic source archive from the tagged commit, computes its checksum, and generates the formula's source metadata. Both architectures must pass before publication.

The build seeds Homebrew's source cache with the exact archive intended for publication, allowing all tests to finish before exposing release assets. `brew bottle --json --no-rebuild` creates the platform bottles. The publisher validates their versions, platforms, filenames and hashes, merges the bottle metadata with Homebrew's tooling, uploads the source/bottles/checksums to a draft release, then publishes it.

Finally, the built-in workflow token commits only the generated `Formula/github-agent-proxy.rb` update to `main`. No cross-repository credential, GoReleaser formula generator or GHCR package is needed. The workflow needs `contents: write` only in the publisher job. If branch protection is added later, adapt this final step to your protected-branch release process.

A failed publication leaves a draft that can be repaired by retrying the publisher job. If release assets were already published, a retry verifies identical contents and can finish the formula update without replacing the assets. A newer formula version is never replaced by an older release. The existing container publication workflow remains independent.
