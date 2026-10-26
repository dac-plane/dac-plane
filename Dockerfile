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
