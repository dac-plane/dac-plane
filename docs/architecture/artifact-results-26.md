# Artifact admission, results and review — issue #26

This is an implementation note for [#26](https://github.com/dac-plane/dac-plane/issues/26),
under the active baseline and ADR-0003. It does not select a production artifact provider.

## Lifecycle and trust

The runtime reports `result.proposed` for a bound native session/turn, exact source
revision and 1–32 evidence declarations. Product identifiers and writer generations
are allocated by the control plane. Ingestion joins the proposal with that turn's
successful end under Attempt/Task locks, regardless of observation order. Ordinary
turn completion, idle or interrupted work does not imply a proposed result.

The Task enters `verifying` and its Attempt `finalizing`. A proposal is immutable:
another stream or delivery cannot replace its source revision or evidence set.
Cancelled, failed and superseded attempts cannot activate or complete verification.

The runtime producer and artifact store have distinct identities. Workers bind
the configured store once, before sending; a different store cannot recover the
same operations after reconfiguration. `WithArtifactStores` supplies this explicit
producer-to-store registry; ordinary runtime registration does not imply storage
authority. Stage operations are excluded from runtime lookup and reconciliation.

## Byte path and durability

Each artifact has stable, distinct operation IDs for `authorize_upload`,
`export_artifacts`, `stat_object` and `commit_manifest`. The stage mapping and send
markers are durable. Calls are bounded by worker timeout, retry policy and a
24-hour artifact-work deadline from activation. The artifact writer generation
is separate from worker/outbox lease epochs and survives worker takeover.

An upload capability is returned in memory and forwarded to the runtime. It is
never stored in PostgreSQL, an outbox, an audit event or a product view. Bytes
travel to provider staging, not through product persistence.

All four methods must support exact stable-operation replay. In particular,
authorization replay must recover a usable upload capability; export replay must
compare manifest/native identity independently of transient capability contents;
stat may report newer staging observations; and commit retry is not a terminal
cached outcome. A committed/fenced outcome is immutable. A provider without these
guarantees cannot qualify for this protocol. This explicit replay contract is
limited to artifact stages, not generalized to governed external actions.

Stat is diagnostic, not admission authority. Commit must hash and freeze the same
object atomically, returning the exact manifest, observed digest/size, immutable
object version and commit time. A lost reply leaves send uncertainty; recovery
replays the same operation to obtain its recorded outcome. Matching bytes alone
cannot reconstruct or substitute for the committed manifest.

The product rechecks scope, live harness qualification, writer generation and
claim ownership before sends and before admission. A stale result transaction
cannot acknowledge the new holder's outbox claim. Late store commitment may exist
externally after cancellation without becoming a product Artifact or result.

Only `committed` inserts the admitted manifest, Artifact and integrity verification
record together. Retry/fenced outcomes remain evidence but create no new Artifact.
Missing bytes and mismatch use bounded retries; exhaustion fails verification.
The byte-backed simulator hashes real fixture bytes and freezes under its commit
mutex. The older scripted simulator remains useful for malformed-provider tests.

## Immutable results and review

Finalization requires every declared artifact's exact manifest and verification
evidence, all from the proposal's Attempt and source revision. One transaction
creates the ResultRevision and verification links, completes the Attempt, releases
its unused budget reservation and moves the Task to `ready_for_review`.

Verification means integrity/provenance admission, not correctness of the patch,
truth of an agent's test report or a platform-executed acceptance check. Evidence
kind/source is retained. No automatic acceptance or arbitrary verifier runner is
introduced. Existing legacy artifacts remain readable but do not acquire invented
verification evidence and cannot satisfy new finalization gates.

`ReviewResult` is owner-authorized, nonce-idempotent and expected-version checked.
It names the exact current revision. Accept/reject are terminal; request changes
preserves the prior review/revision and atomically schedules a new Attempt under
the same Task budget. Fresh scheduling terms cannot enlarge the ceiling. A spend-
governed rework waits for `AssertSpend.ReviewNonce`; the initial submission uses
`SubmissionNonce`. Exactly one scheduling source is permitted.

Invalid scheduling or budget refusal rolls back the review and all new records.
Concurrent dispositions have one winner. Previous manifests, revisions, evidence
and review outcomes are append-only. Rework produces its own artifacts and links
its next revision to the immutable predecessor.

## Migration and remaining gates

Migration 00006 adds staging, stage-purpose, commit/verification and revision-link
records. Downgrade locks affected tables and refuses while any version-6 data or
allocated writer generation exists. It never converts old rows into evidence.

The application acceptance suite drives ingestion, workers and owner review; it
also exercises byte mismatch, TOCTOU, lost replies, claim loss, partial admission,
concurrent streams/reviews, cancellation, shadow mode, lineage and downgrade refusal.
The ordinary migration suite retains empty-database down/up coverage.

The completed local verification is recorded in [tasks/plan.md](../../tasks/plan.md).
The full `just ci` run passed, including uncached race-enabled PostgreSQL integration
tests, vulnerability scanning and workflow lint. A focused mutation test detected
an inverted integrity gate. No production integration or GitHub CI run is claimed.

Broader recovery/security campaigns remain #27/#28. Post-action/failure-hook delivery
and safe typed hook evidence from #25 remain an explicit #30 freeze gate. Nothing
here proves a production artifact store, runtime or deployed environment qualified.
