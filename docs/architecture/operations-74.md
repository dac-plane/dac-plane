# Issue #74 operational evidence

Date: 2026-10-26. Host architecture: arm64. Confidence: high for the observations below; issue
#88 prevents completion of #74.

This note records deployed observations from the issue #74 Compose fixture. It is evidence, not an
availability or provider-qualification claim. The runner used Docker Engine 29.8.2, Docker Compose
v5.5.0 and PostgreSQL 16.15. The old release was built from
`ca20b3e7438707f58d8735251f8ffb8abc5a7a32`.

## Commands and images

The individually runnable entry points are:

```sh
just test-operations-fencing
just test-operations-restart
just test-operations-backup
just test-operations-upgrade
just test-operations
```

The focused runs below used the same tagged Go package and Compose harness after the images had been
built by the recipes. Image identifiers from the observed run were:

| Fixture | Image identifier |
|---|---|
| pinned old control plane | `sha256:c821d027480e1751133488d47c018c157621c6061b73f6a98274f6762a9e3697` |
| current control plane | `sha256:ba7f5b897e0fa3b90bf28fcc61e81e1574b1888d333efc9f8985055de255a627` |
| operations next release | `sha256:c1aa62592d22b24c410188907474f97a69a0f260dfeea9503435dd97f94eb739` |
| operations simulator | `sha256:200c1cdb04c59a8b3c5958fb7955b5c80b860e873321ca8201ae598e934bd8b5` |

These are local OCI image identifiers, not registry digests.

## Observations

### Multi-replica fencing — pass

The after-effect pause was reached on the first claimant
`control-plane-01a17908-d547-76c5-b9bd-26e3a940a2ec`. After only the named outbox and Attempt leases
were expired, `control-plane-01a17908-d55b-7516-9b08-9a53271f70df` won epoch 2. The simulator
recorded one effect. The takeover moved the operation to `uncertain`, and resuming the stale process
did not overwrite that winning settlement. Duration: 22.73 seconds.

### Kill and restart — failed, blocks #74

The simulator reached `before_effect`; the claiming process was killed, its named leases were
expired, and `control-plane-01a17911-0132-71a5-9c05-6c5d330c8269` won outbox epoch 2. The replacement
did not deliver the stable runtime operation: the durable `send_started` marker caused the existing
worker recovery path to move the operation to `uncertain` and the Attempt to `reconciling`. The
simulator ledger contained one delivery, zero effects and no applied effect record. Duration: 28.74
seconds. Failure logs were preserved at
`.work/operations/dac-ops-processkillandrestart.log`.

This is not waived. [Issue #88](https://github.com/dac-plane/dac-plane/issues/88) records the
required recovery-contract decision. The accepted recovery baseline currently forbids blindly
resending an ordinary runtime request after the send marker, while the #74 scenario requires a
replacement delivery despite having no authoritative non-effect receipt. Issue #74 remains open.

### Logical backup and restore — pass

Both serving replicas were stopped at `ready_for_review`. A custom-format `pg_dump` was taken as
`dac_migrate`; the database was dropped, recreated from `template0`, restored as `dac_migrate`, and
both replicas were restarted while the external simulator artifact bytes stayed alive. The restored
task/revision document and artifact metadata matched the pre-backup snapshot. Observed artifact:

```text
artifact: 01a1790f-e51e-7ee4-8901-e1093c71c949
digest: sha256:3e210542b29e2c4f0c6a0aa35e9c5dfe8f58f21f9eff8b1aae321c13ba115874
size: 124 bytes
writer epoch: 1
media type: text/plain
kind: code_change
```

Accepting the restored revision moved the task to `succeeded`. Duration: 35.31 seconds.

### Rolling upgrade — pass

Two pinned-old replicas started against schema 13 and accepted a task. Replica A and then replica B
were recreated with the operations next-release image while PostgreSQL remained at schema 13; an
authenticated task read succeeded after each replacement. Both adapter endpoints reconnected. The
explicit next-image migration advanced the database to fixture schema 14, and both next-release
replicas remained ready. A fresh pinned-old process refused schema 14 with the expected out-of-range
schema error. The original task then completed to `succeeded`.

The simulator recorded two deliveries and two effects for the operation: one `turn_started` effect
and the separately identified `turn_steered` effect required by the attention answer. There was
exactly one start effect across the rolling replacement. Duration: 28.28 seconds.

## Limits

This evidence covers Docker Compose, two control-plane processes and one PostgreSQL primary. The
restore is a logical backup exercise, not database failover. The providers are deterministic
simulators, not qualified external providers. No throughput, RPO, RTO, high-availability or
availability claim follows from these runs. Fixture schema 14 and its migration exist only in the
`operationstest` build and do not change the production migration source.
