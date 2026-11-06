# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY notices.go skill.go THIRD_PARTY_NOTICES.md ./
COPY skills ./skills
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /github-agent-proxy ./cmd/github-agent-proxy

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /github-agent-proxy /github-agent-proxy
COPY THIRD_PARTY_NOTICES.md /THIRD_PARTY_NOTICES.md
USER 10001:10001
EXPOSE 8443
ENTRYPOINT ["/github-agent-proxy"]
