# Local binary development only. CI runs its commands directly.
set shell := ["bash", "-euo", "pipefail", "-c"]

default:
    @just --list

build:
    go build -trimpath -o bin/github-agent-proxy ./cmd/github-agent-proxy

# Real CLI tests use local mocks; no PAT is needed.
test:
    git --version
    gh --version
    go test -race -count=1 -timeout=3m ./...

vet:
    go vet ./...

fmt:
    gofmt -w notices.go cmd internal

check-fmt:
    test -z "$(gofmt -l notices.go cmd internal)"
