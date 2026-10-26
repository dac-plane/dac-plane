# Issue #73 — Ship the container image, health checks and migration rollout

Tracker: https://github.com/dac-plane/dac-plane/issues/73
Branch: `feature/73-deployable-unit`, from `main` at `fc11489`, in the main checkout.
Parent: #65 (M2: Deployable control plane).

Confirmed with the maintainer on 2026-10-25: one PR closes #73; the stack smoke drives the full
lifecycle (architecture-explore `docs/implementation-status.md`, SPEC §12, A01/A02); the development
profile uses a digest-pinned mock-oauth2-server over TLS with real client-credentials tokens (SPEC
workload identity), and the kit gains a client-credentials token source.

## Gaps this issue closes

Gaps found that #73 must close for the deployed lifecycle to work:
1. **Nothing follows live event streams.** The reconciler subscribes only to uncertain operations
   (`internal/worker/reconcile.go`, `store.UncertainOperations`), and `application.Ingest` has no
   production caller; tests inject events directly. ADR-0010 already says the product side subscribes
   and resumes from the durable cursor, so this is implementation, not a new decision.
2. **No trusted way to qualify a harness.** `store.UpsertHarness` is called only from tests; dispatch
   needs a live harness.
3. **`dac-sim-adapter` scripts fixed operation IDs and serves only the runtime role.** In a stack, IDs
   are generated at submission and decisions at runtime, and results need an artifact store.
4. **No dependency-exclusion check** exists yet; ADR-0009 assigns it here.

## Work, in order

### 1. Housekeeping
- On `main`, create `feature/73-deployable-unit` in the main checkout. Rewrite `tasks/plan.md` for
  #73 and commit it.

### 2. Configuration and secrets (`internal/config`)
- Every secret-bearing variable also accepts `<NAME>_FILE`; giving both is an error. This covers
  `DAC_DATABASE_URL` and the TLS key. Values never appear in errors (existing test style).
- New settings:
  - `DAC_SURFACE_ADDR` (default `:8443`);
  - `DAC_ADAPTER_ADDR` (default `:9443`);
  - `DAC_OPS_ADDR` (default `127.0.0.1:8081`);
  - `DAC_TLS_CERT_FILE` and `DAC_TLS_KEY_FILE`, required by `serve`;
  - `DAC_DEPLOYMENT`, either `production` (default) or `development`.
- An issuer in the identity config may carry `"development": true`, added to
  `identity-config.schema.json` and `authn.IssuerConfig` with an agreement test. `serve` refuses that
  issuer unless `DAC_DEPLOYMENT=development`, which enforces ADR-0009's "refused by production
  configuration".

### 3. Process subcommands (`cmd/dac-plane`)
Structure: one small file per subcommand with `run(args)` returning an exit code, like
`provision.Run`.

- **`serve`** (also the default for compatibility):
  - Checks the schema range first: `migrations.MinServeVersion` and `migrations.CurrentVersion`,
    read via the goose provider's DB version. Outside the range it exits with a clear error.
  - Then the bootstrap composes:
    - `authn.LoadConfig`, then `authn.New` for the Surface and `authn.NewVerifier(AdapterIssuers)`
      for adapters;
    - `httpapi.New` and `adapterwire.NewEndpoint` over the existing registry;
    - the worker.
  - Three `http.Server`s:
    - Surface and adapter use TLS 1.2+ from the mounted files. `GetCertificate` reloads the pair when
      the files change, so an operator replaces them without a restart.
    - The operations listener serves plaintext only on loopback and TLS otherwise, so no plaintext
      reaches a non-loopback address.
  - Graceful shutdown on SIGTERM: stop accepting, close the registry, drain the worker.
- **`migrate up | status | down-to N`**: the goose provider with `lock.NewPostgresSessionLocker`
  (advisory lock), so a concurrent run waits. `down-to` keeps the existing refusal when evidence would
  be lost.
- **`health live | ready`**: dials the loopback operations listener and exits 0 or 1, so a container
  needs no shell.
- **`provision`**: unchanged, plus `qualify-harness -tenant -provider -mode live|shadow` (gap 2), with
  validation and an audit row like the other provisioning operations, and CLI usage tests.

### 4. Operations listener (`internal/bootstrap` or a small `internal/ops`)
- `/livez`: the process loop is running.
- `/readyz`: database ping, schema version in range, both TLS listeners accepting, and the worker
  started.
- Bodies are status only; a test asserts they leak no configuration.

### 5. Observation follower (`internal/worker`, gap 1)
- `Observe(ctx)` runs in `worker.Run` next to dispatch and reconcile. For each tenant it lists
  accepted runtime dispatch operations of live attempts (a new store query beside
  `UncertainOperations`).
- For each one whose port has a live connection on this replica, it subscribes from the durable
  `events` cursor, ingests each frame with the same binding and cursor rules as `application.Ingest`,
  and stops after `StreamTimeout`. The next pass resumes from the durable cursor; rehydration follows
  the reconciler's one-reset-per-pass rule.
- Several replicas following one stream is safe because ingestion is idempotent (cursor and natural
  keys in one transaction). #74 proves it.
- Integration tests:
  - attention, decision consumption, result proposal and turn finish arrive through the wire with no
    direct `Ingest`;
  - a forged event is refused;
  - after a disconnect, following resumes from the cursor.

### 6. Simulator adapter for a stack (gap 3)
- `internal/simulator`:
  - `RuntimeScenario.Default` answers any start operation.
  - `DefaultDecision` answers any steer.
  - A decision-gated stream continuation streams `decision.consumed` (naming the delivered decision),
    then `result.proposed` with the artifact, then `turn.finished`.
  - Unit tests cover all three.
- `cmd/dac-sim-adapter`:
  - The scenario file gains `default` and an optional artifact store.
  - One process serves the runtime and the artifact store as two kit connections sharing the byte
    store, as `WithArtifactExport` does in tests.
  - It takes `DAC_ADAPTER_TOKEN_*` client-credentials settings.
  - The conformance fixture keeps working.

### 7. Kit client credentials (`adapterkit`)
- `ClientCredentials(TokenURL, ClientID, SecretFile, Scope, HTTPClient) TokenSource` on
  `golang.org/x/oauth2/clientcredentials`, at its latest version older than two weeks. Expiry comes
  from `expires_in`; the secret is read from a file and never logged.
- Tests against an `httptest` token endpoint.

### 8. Image (`Dockerfile`, multi-target)
- Builder: `golang` at the go.mod toolchain, digest-pinned, with `CGO_ENABLED=0`, `-trimpath` and
  `-ldflags "-s -w -buildid="`. `-mod=readonly` uses the vendored-free module cache.
- Targets:
  - `dac-plane`: `gcr.io/distroless/static-debian12:nonroot` by digest, `USER nonroot`, entrypoint
    `/dac-plane`;
  - `dac-sim-adapter`: the same base.
- `.dockerignore` excludes `.work`, `architecture-explore` and `node_modules`.
- Reproducibility: `SOURCE_DATE_EPOCH` from the last commit, built with buildx
  `rewrite-timestamp=true`, which current BuildKit and reproducible-builds.org guidance recommend.

### 9. Reference Compose stack (`deploy/compose/`)
- **Base `compose.yaml`:**
  - PostgreSQL 16, digest-pinned, on a volume. An init script creates a `dac_migrate` owner role and a
    `dac_serve` DML-only role, proving `serve` needs no DDL.
  - One-shot `migrate`; `serve` depends on its completion.
  - `serve` replicas: `read_only: true`, secrets mounted as files, healthcheck
    `["CMD", "/dac-plane", "health", "ready"]`.
  - No adapters and no artifact store.
- **Profile `dev`:**
  - `issuer` (mock-oauth2-server 6.0.5 by digest, TLS from a keystore, request mappings for adapter
    and user clients);
  - `sim-adapter` (runtime plus artifact store);
  - a `provision` one-shot that creates the tenant, principals, bindings, provider registrations,
    adapter bindings and a live harness;
  - the dev identity config with `"development": true`.
- **Dev PKI:** a `just` recipe generates a throwaway CA and the server and issuer certificates with
  `openssl` into `.work/stack/`. Nothing secret is committed. Containers trust the CA through
  `SSL_CERT_FILE`, so no code change is needed.
- **`deploy/compose/README.md`** (operator documentation ADR-0009 requires): configuration table,
  database privileges, upgrade sequence (migrate, then roll `serve`), the expand-then-contract rule,
  rollback within range, and what the dev profile is not.

### 10. Stack smoke test (`internal/stacktest`, build tag `stack`)
- A Go test that drives the running dev stack only through HTTPS. It fetches user tokens from the
  issuer; a dialer maps service names to published ports so `iss` and TLS names match.
- Sequence:
  1. submit;
  2. the operation is accepted over the wire by the separate-process simulator;
  3. the attention request appears in the event stream;
  4. the decision is posted and consumed;
  5. a result revision appears with its artifact;
  6. accept is reviewed and the task succeeds.
- Also checked: `/readyz` through `health`, and `serve` refusing to start against a schema outside its
  range.

### 11. Recipes, CI, docs
- justfile:
  - `deps-check` (`go list -deps ./cmd/dac-plane` contains no `internal/simulator`,
    `internal/testsupport` or `adapterkit/conformance`);
  - `image-build`;
  - `image-repro` (build twice, compare digests);
  - `stack-certs`, `stack-up`, `stack-down`;
  - `test-stack` (up, smoke, down, logs on failure).
  - Add `deps-check` to `ci`. Extend `vet-integration` to the `stack` tag.
- `.github/workflows/ci.yml`: a `stack` job (buildx docker-container driver, `image-repro`,
  `test-stack`) calling the same recipes; actions pinned by SHA as today.
- Docs:
  - `CLAUDE.md` working set: `deploy/`, `Dockerfile`, `internal/stacktest`; update the "no process
    serves it" sentence.
  - `docs/architecture/README.md` deployment note, and `deployment-view.drawio` only if the ports or
    services drawn differ, then re-export.
  - `contract-freeze-30.md` row for the identity-config `development` flag.
  - `tasks/plan.md` deviations.

## Verification
- Unit:
  - config `_FILE` handling and leak tests;
  - the identity-config `development` agreement test;
  - CLI usage for `migrate`, `health` and `qualify-harness`;
  - simulator default and decision-gated scripts;
  - kit `ClientCredentials`.
- Integration (`-count=1`, Docker):
  - `migrate` concurrency waits on the lock;
  - `serve` refuses an out-of-range schema;
  - readiness flips with the database;
  - the observation follower suite over `wiretest`;
  - `qualify-harness`.
- Mutation evidence:
  - drop the follower from `worker.Run`: the smoke stalls at the attention step;
  - remove the schema-range check: the out-of-range test serves;
  - let the dev issuer through in production: the config test fails.
- `just image-repro` (identical digests), `just deps-check`, and `just test-stack` (full lifecycle
  green).
- Final gate: `just ci` green. Open the PR with the governance template, bind it with ccd_pr tools,
  read CI and offer Auto-fix, and handle review-bot findings as before.

## Deviations

- The development bootstrap is expressed as ordered one-shot provisioning services rather than one
  shell-driven container because the release image is distroless and intentionally contains no shell.
- Default simulator stream references use `${operation_id}` placeholders. The runtime binds them and
  recomputes payload hashes per operation so repeated stack submissions cannot collide on provider
  native references.
