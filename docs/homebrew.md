# Homebrew bottles

This repository is both the source repository and a Homebrew tap. [gobottle](https://github.com/isometry/gobottle) cross-compiles the proxy and packages Homebrew bottles for Apple Silicon macOS 15 or newer. Bottles are hosted on GHCR; the generated formula stays in this repository. Intel bottles are not built or tested.

```sh
brew tap jacobfg/github-agent-proxy https://github.com/jacobfg/github-agent-proxy
brew trust --formula jacobfg/github-agent-proxy/github-agent-proxy
brew install jacobfg/github-agent-proxy/github-agent-proxy
github-agent-proxy --version
```

Older Homebrew versions without `brew trust` can skip that command. Until the first stable release is published, use `brew install --HEAD jacobfg/github-agent-proxy/github-agent-proxy` after the packaging changes are merged. The generated formula retains source-build and HEAD installation support.

## What is installed

The package installs only the proxy binary. Third-party notices are embedded and available with `github-agent-proxy --licenses`. Go is needed only when building from source. No policy file is installed. Run `github-agent-proxy init-policy --output policy.json` to create a new read-only policy, then configure it using the [policy guide](policy.md). The default JSON is embedded in the binary; existing files are never overwritten.

There is no Homebrew service or launchd definition. Installation does not create credentials, configure certificate trust, or start a process.

## Environment integration

The consumer owns proxy lifecycle, policies, credentials, TLS trust and agent configuration. Local Safehouse integration and migration tools live in `agent-safehouse-custom/proxy/`; the formula does not install or run them.

## Build and release

`.gobottle.yaml` defines the build, bottle platform, formula tests, registry and tap. Edit that configuration rather than the generated formula after the first release. The workflow pins gobottle, which is currently experimental; Renovate proposes version updates.

Pull requests run the Go race suite and vet, cross-compile with `CGO_ENABLED=0` on Linux, and generate a bottle and preview formula without publishing. An Apple Silicon macOS runner installs the bottle from Homebrew's checksum-verified cache and runs `brew test`, checking the version, embedded notices, default policy generation and CA generation. It does not build a Go toolchain on macOS.

gobottle v0.9 labels macOS bottles with Homebrew's oldest available tag (`arm64_monterey`). Homebrew can install that bottle on newer macOS; our supported and tested baseline is macOS 15.

After merging to `main`, push a stable `x.y.z` tag, without a `v` prefix. The workflow checks that the tagged commit belongs to `main`. Source URLs pin that commit and include a version query for Homebrew's version detection. Development builds use `0.0.0` and cannot enter the publisher job.

Only after the tests and bottle installation pass does `gobottle push` upload the tested artifact to GHCR and `gobottle release` update `Formula/github-agent-proxy.rb` on `main`. The built-in workflow token needs `packages: write` and `contents: write` only in the publisher job. No separate tap token or custom Python release scripts are used. If branch protection is added, adapt the formula update to that process.

After the first push, change the GHCR package `jacobfg/github-agent-proxy/github-agent-proxy` to **public** in its package settings so Homebrew can download it without credentials. This is a one-time owner action; repository visibility alone does not make a new package public.

To retry a failed publication, rerun only the publisher job while its artifacts are retained. gobottle reuses those bottles and avoids committing an unchanged formula. Do not rerun old release publications after a newer version has shipped: gobottle does not prevent downgrading the formula. The existing container publication workflow remains independent.
