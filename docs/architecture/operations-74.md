# Issue #74 operational evidence

Date: 2026-10-26. Host architecture: arm64. Confidence: high for the observations below.

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
| pinned old control plane | `sha256:39b650be20aa2ec43423012db49b57eb174813591d32d01f8b15afc237aab3bb` |
| current control plane | `sha256:d2bb5b434a86c7902ed4f5587d1b9b8e94c866f9682d0b2ffe8b43ccc7d2776e` |
| operations next release | `sha256:986d463432bdb515b440ff7ee6c208aa74c7d16c00e72f1eaef9106f916cbe0e` |
| operations simulator | `sha256:325cdf1c7ee08bd29ef9bba968a54dc91d54078939a2cc5917ea460b38ceda47` |

These are local OCI image identifiers, not registry digests.

## Observations

### Multi-replica fencing — pass

The after-effect pause was reached on the first claimant
`control-plane-01a1793b-ea94-731b-bede-d6ffc485c226`. After only the named outbox and Attempt leases
were expired, `control-plane-01a1793b-eaa4-7ea7-9b85-99a98a711a2e` won epoch 2. The negotiated
`operation.replay` delivery returned the simulator's recorded result without another effect. The
winning operation was `accepted`; resuming the stale process did not overwrite the epoch-2
settlement. The ledger recorded at least two deliveries and exactly one effect. Duration: 22.70
seconds.

### Kill and restart — pass

The simulator reached `before_effect`; the claiming process was killed and only its named leases
were expired. The replacement replica won outbox epoch 2 and used the capability-gated replay path.
The abandoned first delivery applied no effect; the replacement delivery applied exactly one effect,
settled the operation as `accepted`, and cursor processing reached attention. The killed replica was
restarted and became healthy without losing durable state. Duration: 29.43 seconds.

The initial failing run is retained as the evidence that motivated
[ADR-0012](../adr/0012-negotiate-stable-operation-replay.md) and
[issue #88](https://github.com/dac-plane/dac-plane/issues/88): one delivery, zero effects and an
uncertain operation before capability-gated replay was implemented. The passing behavior does not
generalize to adapters that do not negotiate `operation.replay`; those retain uncertain
reconciliation.

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

The aggregate `just test-operations` run passed all four scenarios in 113.554 seconds. The same
recipe is the blocking CI entry point.

This evidence covers Docker Compose, two control-plane processes and one PostgreSQL primary. The
restore is a logical backup exercise, not database failover. The providers are deterministic
simulators, not qualified external providers. No throughput, RPO, RTO, high-availability or
availability claim follows from these runs. Fixture schema 14 and its migration exist only in the
`operationstest` build and do not change the production migration source.
