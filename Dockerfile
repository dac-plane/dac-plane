# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e

# Release identity (ADR-0018). `just release-image` sets these from the tag.
ARG VERSION=dev
ARG REVISION=unknown
ARG KIT_VERSION=dev

FROM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS build

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
ARG VERSION
ARG REVISION
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -mod=readonly -trimpath -ldflags="-s -w -buildid= -X main.version=${VERSION} -X main.revision=${REVISION}" \
    -o /out/dac-plane ./cmd/dac-plane && \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -mod=readonly -trimpath -tags=operationstest -ldflags="-s -w -buildid=" \
    -o /out/dac-plane-operationstest ./cmd/dac-plane && \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -mod=readonly -trimpath -ldflags="-s -w -buildid=" \
    -o /out/dac-sim-adapter ./cmd/dac-sim-adapter

FROM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS codex-adapter-build

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
    curl --proto '=https' --proto-redir '=https' -fL --retry 3 -o /tmp/codex-app-server.tar.gz \
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

FROM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS e2b-adapter-build

WORKDIR /src
COPY adapterkit/go.mod adapterkit/go.sum ./adapterkit/
COPY adapters/e2b/go.mod adapters/e2b/go.sum ./adapters/e2b/
RUN --mount=type=cache,target=/go/pkg/mod \
    cd adapters/e2b && go mod download

COPY adapterkit ./adapterkit
COPY adapters/e2b ./adapters/e2b

ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    test "${TARGETOS}" = linux && \
    cd adapters/e2b && \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -mod=readonly -trimpath -ldflags="-s -w -buildid=" \
    -o /out/dac-e2b-adapter ./cmd/dac-e2b-adapter

FROM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS s3-adapter-build

WORKDIR /src
COPY adapterkit/go.mod adapterkit/go.sum ./adapterkit/
COPY adapters/s3/go.mod adapters/s3/go.sum ./adapters/s3/
RUN --mount=type=cache,target=/go/pkg/mod \
    cd adapters/s3 && go mod download

COPY adapterkit ./adapterkit
COPY adapters/s3 ./adapters/s3

ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    test "${TARGETOS}" = linux && \
    cd adapters/s3 && \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -mod=readonly -trimpath -ldflags="-s -w -buildid=" \
    -o /out/dac-s3-adapter ./cmd/dac-s3-adapter

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS dac-plane

ARG VERSION
ARG REVISION
ARG KIT_VERSION
LABEL org.opencontainers.image.title="dac-plane" \
      org.opencontainers.image.source="https://github.com/dac-plane/dac-plane" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      io.dac-plane.adapterkit.version="${KIT_VERSION}"
COPY LICENSE NOTICE third_party/THIRD_PARTY_NOTICES.txt /licenses/
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

# Published. Commands run in the task's sandbox (ADR-0016), so it needs no shell.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS dac-codex-adapter

ARG VERSION
ARG REVISION
ARG KIT_VERSION
LABEL org.opencontainers.image.title="dac-codex-adapter" \
      org.opencontainers.image.source="https://github.com/dac-plane/dac-plane" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      io.dac-plane.adapterkit.version="${KIT_VERSION}"
COPY LICENSE NOTICE third_party/THIRD_PARTY_NOTICES.txt /licenses/
COPY third_party/codex-app-server /licenses/codex-app-server
COPY --from=codex-adapter-build --chown=nonroot:nonroot /out/dac-codex-adapter /dac-codex-adapter
COPY --from=codex-adapter-build --chown=nonroot:nonroot /out/codex-app-server /codex-app-server
USER nonroot:nonroot
ENTRYPOINT ["/dac-codex-adapter"]

# Never published. Codex runs commands in this container, for the recipes that
# qualify the runtime without a sandbox.
FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc AS dac-codex-adapter-local

COPY --from=codex-adapter-build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=codex-adapter-build --chown=65532:65532 /out/dac-codex-adapter /dac-codex-adapter
COPY --from=codex-adapter-build --chown=65532:65532 /out/codex-app-server /codex-app-server
USER 65532:65532
ENTRYPOINT ["/dac-codex-adapter"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS dac-e2b-adapter

ARG VERSION
ARG REVISION
ARG KIT_VERSION
LABEL org.opencontainers.image.title="dac-e2b-adapter" \
      org.opencontainers.image.source="https://github.com/dac-plane/dac-plane" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      io.dac-plane.adapterkit.version="${KIT_VERSION}"
COPY LICENSE NOTICE third_party/THIRD_PARTY_NOTICES.txt /licenses/
COPY --from=e2b-adapter-build --chown=nonroot:nonroot /out/dac-e2b-adapter /dac-e2b-adapter
USER nonroot:nonroot
ENTRYPOINT ["/dac-e2b-adapter"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS dac-s3-adapter

ARG VERSION
ARG REVISION
ARG KIT_VERSION
LABEL org.opencontainers.image.title="dac-s3-adapter" \
      org.opencontainers.image.source="https://github.com/dac-plane/dac-plane" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      io.dac-plane.adapterkit.version="${KIT_VERSION}"
COPY LICENSE NOTICE third_party/THIRD_PARTY_NOTICES.txt /licenses/
COPY --from=s3-adapter-build --chown=nonroot:nonroot /out/dac-s3-adapter /dac-s3-adapter
USER nonroot:nonroot
ENTRYPOINT ["/dac-s3-adapter"]
