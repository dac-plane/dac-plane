# ADR-0009: Ship one container image, migrated by an explicit step, on a reference Compose stack

- Status: Accepted; schema-rollout ordering superseded by
  [ADR-0011](0011-roll-compatible-binaries-before-schema-migrations.md)
- Date: 2026-10-22
- Issue: [#68](https://github.com/dac-plane/dac-plane/issues/68)

## Context

[ADR-0002](0002-initial-application-boundary.md) builds one control-plane application whose API and
background loops may run in one process, and allows a process split only when evidence meets one of
its triggers. [ADR-0007](0007-surface-api-transport-and-authentication.md) adds the Surface API, forbids
plaintext except on loopback for development, and leaves TLS placement to this decision.
[ADR-0008](0008-adapter-process-boundary-and-wire-transport.md) adds the adapter endpoint, keeps
in-process ports out of production, and leaves to this decision how a production build enforces that
and how replicas expose the endpoint. The
[pre-implementation review](../architecture/review-2026-10-15.md) lists the deployment substrate as
open: "Kubernetes is not assumed".

The process today (`cmd/dac-plane`) reads `DAC_DATABASE_URL` and a connect timeout, opens a PostgreSQL
pool and runs the worker. It does not apply migrations, serves no listener or health endpoint, and has
no image. Each process takes a random worker owner identity, and dispatch, reconciliation and
maintenance claims are fenced, so several processes can already share one database without a second
lifecycle owner. Migrations are embedded goose migrations; an unsafe downgrade is refused.

Grok Bot is operated by its vendor in the vendor's cloud and offers no self-hosted deployment model to
reuse. Its single update lease and claimed upgrade schedules
([master record](../../architecture-explore/plane-bot-explore/notes/MASTER-RECORD.md) §5) show one
updater at a time. The exploration's qualification gate CP9
([candidate qualification](../../architecture-explore/docs/qualification/control-plane-candidates.md))
requires that a foundation "can be self-hosted or operated in the target environment with acceptable
secrets, auth and upgrade story" and that "local-only demos do not pass". It counted a
Kubernetes-only production topology against one candidate and credited plausible self-hosted startup
with PostgreSQL for another. The historical
[specification](../../architecture-explore/docs/spec/SPEC.md) asks for operator setup and upgrade time
and rollback behavior to be measured. dac-plane is held to the same bar it applied to candidates.

## Decision

### Deployment unit

Ship one OCI image containing the single `dac-plane` binary. The image is built reproducibly from a
digest-pinned base, contains no shell or package manager, runs as a non-root user with a read-only root
filesystem, and holds no configuration secrets. The same image runs every role below.

The binary exposes subcommands: `serve` runs the control plane, `migrate` applies schema migrations,
`provision` performs the trusted provisioning ADR-0007 defines, and `health` probes the local
operations listener for liveness or readiness so a substrate health check needs no shell in the
image. Nothing else is a separate artifact. Adapters are separate programs with their own images, built on the adapter kit.

### Roles per process

Every `serve` process runs all roles: the Surface API, the adapter endpoint and the worker. This follows
ADR-0002; separate API and worker processes require one of its split triggers to be proven and a new
decision. Scaling is by running more identical replicas against the same PostgreSQL primary. The
Surface API holds no session state, so any replica serves any request and any event stream. Adapter
connections land on any replica, and a replica claims a provider's work only while it holds a
connection for it, as ADR-0008 decides.

### Schema migrations

Migrations are applied only by `dac-plane migrate`, run as a one-shot step before serving processes of
a new release start. It takes a PostgreSQL advisory lock, so a second concurrent run waits rather than
racing. Serving processes never apply migrations and need no DDL privileges; the operator may give
`migrate` and `serve` different database roles.

Each release declares the range of schema versions it can serve. A `serve` process checks the schema
version at startup and refuses to start, with a clear error, outside that range. Within a release line
migrations are additive, so the previous release keeps serving while the next one rolls out. A
destructive change is split across two releases: expand in one, contract in a later one once no
serving release depends on the old shape. Rolling a binary back is allowed while the schema stays in
its range; a schema downgrade is an explicit `migrate` operation and keeps refusing when it would
discard evidence.

### TLS

dac-plane terminates TLS itself on the Surface API and the adapter endpoint, from certificate and key
files the operator mounts and replaces without rebuilding the image. An operator may also run a proxy
or load balancer in front, but the hop from that proxy to dac-plane must be TLS as well. The same rule
covers the operations listener below. No listener serves plaintext on a non-loopback interface, which
keeps ADR-0007's rule unchanged and needs no exception.

### Health and readiness

A separate operations listener serves liveness and readiness. Liveness reports that the process loop
runs. Readiness additionally requires a reachable database, a schema version inside the release's
range, and the Surface API and adapter listeners accepting connections. The operations listener is
bound to loopback by default and exposes status only, never records, identities or configuration. An
operator may bind it to a private interface for an external health checker; off loopback it serves
TLS from the same mounted certificate material, so no listener serves plaintext off loopback. A
substrate health check inside the container uses the `health` subcommand, which dials the loopback
listener, so the default configuration needs neither a shell in the image nor a listener off
loopback.

### Configuration and secrets

Configuration comes from environment variables; every secret-bearing setting can instead be read from a
file, so secrets arrive through the substrate's secret mechanism and never through the image, a command
line or a log. This covers the database URL, TLS key material and identity-provider client secrets used
by `provision`. Trusted issuers, audiences and token-lifetime ceilings from ADR-0007 and ADR-0008 are
configuration, not code.

### In-process ports excluded from production

The release binary does not link the simulator packages or any other in-process provider port. A CI
check asserts that the dependency graph of `cmd/dac-plane` excludes them. The simulators run in tests
and, in the development profile below, as out-of-process reference adapters in their own image.

### First substrate: a reference Compose stack

The first supported substrate is a reference Docker Compose stack, maintained in this repository and
exercised in CI. It is a production-capable pilot deployment, not a demonstration:

- PostgreSQL 16 from a digest-pinned image, with a persistent volume;
- a one-shot `migrate` service that must complete before `serve` starts;
- one or more `serve` replicas with mounted TLS material and secret files;
- no artifact store and no provider adapters until providers are qualified, because the stack must not
  imply qualification that does not exist.

A separate development profile adds the out-of-process simulator adapters and a local test identity
issuer. That profile is refused by production configuration, and its issuer must never be trusted
outside it.

Kubernetes packaging is added only when an operator requirement or measured evidence calls for it,
with its own decision. Nothing in the image or the binary depends on Compose.

### Operator responsibilities

The operator provides and operates the PostgreSQL primary, its backups and failover, TLS certificates,
the OpenID Connect provider and its client credentials, and the secret store. dac-plane documents the
required database privileges, configuration and upgrade sequence. Backup, restore and multi-replica
behavior are proven by the operations evidence issue before they are claimed.

## Alternatives considered

- **Migrate on startup under an advisory lock:** rejected because every serving replica would need DDL
  privileges and a rollout would couple schema change to process restarts; a failed migration would
  surface as a crash loop instead of a failed upgrade step.
- **Separate API and worker processes now:** rejected because no ADR-0002 split trigger is proven.
- **Kubernetes first:** rejected because the exploration's own deployment-fit bar counted a
  Kubernetes-only topology against a candidate, and it would make a cluster a prerequisite for a pilot.
- **Release binaries with system service units only:** rejected as the first path because an image
  gives one reproducible artifact for every substrate; binaries can be published later from the same
  build.
- **TLS terminated only at an operator proxy:** rejected because dac-plane would listen in plaintext on
  a non-loopback interface, contrary to ADR-0007.
- **A Compose file as a local demo only:** rejected because CP9 states that local-only demos do not
  pass, and the same bar applies here.
- **Simulators selectable by configuration in the release binary:** rejected because a configuration
  mistake could satisfy production dispatch with no real effect; exclusion from the binary is checked
  instead.

## Consequences

- #73 adds the subcommands, the startup schema-range check, the TLS and operations listeners,
  file-based secrets, the image, the reference stack with its development profile, and CI recipes that
  build the image reproducibly, check the dependency exclusion, and run the stack end to end.
- `cmd/dac-plane` gains subcommands; the current single-mode entry point becomes `serve`, and the
  reference stack's health checks run `health` inside each container.
- Migrations gain a release compatibility range and the expand-then-contract rule; a migration that
  breaks the previous release must be split.
- #74 proves rolling upgrade across a migration, process kill and restart, backup and restore, and
  claim fencing with several replicas and moving adapter connections.
- Until #75 qualifies providers, the reference stack runs the lifecycle only with the development
  profile's simulators; a pilot without qualified providers has no effectful dispatch.
- PostgreSQL high availability, backup tooling, observability export and resource sizing remain the
  operator's choices and need their own evidence before dac-plane recommends any.
