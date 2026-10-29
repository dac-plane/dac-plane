# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e

FROM golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af AS build

WORKDIR /src

COPY go.mod go.sum ./
COPY adapterkit/go.mod adapterkit/go.sum ./adapterkit/
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download && \
    cd adapterkit && go mod download

COPY adapterkit ./adapterkit
COPY cmd ./cmd
COPY contracts ./contracts
COPY internal ./internal

ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -mod=readonly -trimpath -ldflags="-s -w -buildid=" \
    -o /out/dac-plane ./cmd/dac-plane && \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -mod=readonly -trimpath -tags=operationstest -ldflags="-s -w -buildid=" \
    -o /out/dac-plane-operationstest ./cmd/dac-plane && \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -mod=readonly -trimpath -ldflags="-s -w -buildid=" \
    -o /out/dac-sim-adapter ./cmd/dac-sim-adapter

FROM golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af AS codex-adapter-build

WORKDIR /src
COPY adapterkit/go.mod adapterkit/go.sum ./adapterkit/
COPY adapters/codex/go.mod adapters/codex/go.sum ./adapters/codex/
RUN --mount=type=cache,target=/go/pkg/mod \
    cd adapters/codex && go mod download

ARG TARGETARCH
ARG CODEX_APP_SERVER_VERSION=0.165.0
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) archive=codex-app-server-x86_64-unknown-linux-musl.tar.gz; expected=5f6940dae95bc711b699f57c4edba522b7de682e267901c37a03038c3b440fc0 ;; \
      arm64) archive=codex-app-server-aarch64-unknown-linux-musl.tar.gz; expected=431e82c7dfa08c2aa97cdd637d09324557c8de8140279184259eefbe871e4e26 ;; \
      *) echo "unsupported Codex architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fL --retry 3 -o /tmp/codex-app-server.tar.gz \
      "https://github.com/openai/codex/releases/download/rust-v${CODEX_APP_SERVER_VERSION}/${archive}"; \
    echo "${expected}  /tmp/codex-app-server.tar.gz" | sha256sum -c -; \
    mkdir -p /out; \
    tar -xzf /tmp/codex-app-server.tar.gz -C /out; \
    mv "/out/${archive%.tar.gz}" /out/codex-app-server; \
    chmod 0755 /out/codex-app-server

COPY adapterkit ./adapterkit
COPY adapters/codex ./adapters/codex

ARG TARGETOS
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    test "${TARGETOS}" = linux && \
    cd adapters/codex && \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -mod=readonly -trimpath -ldflags="-s -w -buildid=" \
    -o /out/dac-codex-adapter ./cmd/dac-codex-adapter

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS dac-plane

COPY --from=build --chown=nonroot:nonroot /out/dac-plane /dac-plane
USER nonroot:nonroot
ENTRYPOINT ["/dac-plane"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS dac-plane-operationstest

COPY --from=build --chown=nonroot:nonroot /out/dac-plane-operationstest /dac-plane
USER nonroot:nonroot
ENTRYPOINT ["/dac-plane"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS dac-sim-adapter

COPY --from=build --chown=nonroot:nonroot /out/dac-sim-adapter /dac-sim-adapter
USER nonroot:nonroot
ENTRYPOINT ["/dac-sim-adapter"]

FROM debian:bookworm-slim@sha256:99c0b25c46d0214d5a2091092e7908907dcaecd6a962cde0ac700acee222b6f3 AS dac-codex-adapter

COPY --from=codex-adapter-build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=codex-adapter-build --chown=65532:65532 /out/dac-codex-adapter /dac-codex-adapter
COPY --from=codex-adapter-build --chown=65532:65532 /out/codex-app-server /codex-app-server
USER 65532:65532
ENTRYPOINT ["/dac-codex-adapter"]
