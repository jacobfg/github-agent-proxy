# syntax=docker/dockerfile:1
ARG RUST_VERSION=1.99

FROM --platform=$BUILDPLATFORM rust:${RUST_VERSION}-alpine AS build
ARG TARGETARCH
WORKDIR /build
RUN apk add --no-cache musl-dev
COPY Cargo.toml Cargo.lock ./
COPY src ./src
# Pure-rust TLS (rustls + webpki-roots) keeps the final image scratch-clean:
# no openssl, no system CA bundle needed.
RUN --mount=type=cache,target=/usr/local/cargo/registry,id=cargo-registry-$TARGETARCH \
    --mount=type=cache,target=/usr/local/cargo/git,id=cargo-git-$TARGETARCH \
    --mount=type=cache,target=/build/target,id=cargo-target-$TARGETARCH \
    cargo build --release --locked \
    && cp target/release/github-agent-proxy /usr/local/bin/github-agent-proxy

FROM scratch
COPY --from=build /usr/local/bin/github-agent-proxy /github-agent-proxy
COPY THIRD_PARTY_NOTICES.md /THIRD_PARTY_NOTICES.md
USER 10001:10001
EXPOSE 8443
# Container default listens on all interfaces (container networking is the
# boundary); host runs should override to 127.0.0.1.
ENV GAP_LISTEN=0.0.0.0:8443
HEALTHCHECK --interval=30s --timeout=3s CMD ["/github-agent-proxy", "healthcheck", "--insecure"]
ENTRYPOINT ["/github-agent-proxy"]
