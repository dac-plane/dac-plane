# ADR-0026: Keep question and answer text as product records, served to the task owner

- Status: Accepted
- Date: 2026-11-18
- Issue: [#157](https://github.com/dac-plane/dac-plane/issues/157)
- Refines: [ADR-0003](0003-provider-contract-semantics.md), [ADR-0022](0022-govern-connectors-at-a-control-plane-gateway.md)

## Context

A runtime can ask a person a question in words, but dac-plane keeps none of the words. An attention request records
its kind, responders, expiry and, for an approval, the digest of the exact action. A decision of kind `answer`
records only `answer_digest`, "stored outside the decision record" (`internal/lifecycle/attention.go`), and no
Surface API operation accepts that content. The worker steers the runtime with the decision, so a runtime that asked
a question never receives the answer's text, and a surface cannot show the question.

What exists:

- The kind `question` and the decision `answer` are in the domain, the Surface API and the adapter protocol. No real
  runtime raises a question: the Codex adapter emits only `action_approval` and its `steer` refuses every decision
  except `approve_once` and `deny`.
- Codex app-server 0.165.0, the pinned version, sends the server request `item/tool/requestUserInput` with
  `questions[]` (`id`, `header`, `question`, optional `options[]`, `isOther`, `isSecret`) and `isBlocking`, and expects
  `{answers: {<question id>: {answers: [string]}}}`. The request is marked experimental in the protocol schema.
- The connector gateway already serves a proposal's exact arguments to the task owner only, bound to the attention
  version, from a product table (`GetConnectorProposal`).
- A request or reply result is bounded to 64 KiB, and a frame to 1 MiB.

## Decision

**Question and answer text are product records in PostgreSQL.** A question is stored once per attention request; an
answer once per decision. Both are short, a surface must read the question back, and the worker must deliver the
answer, which is what the product database is for. The artifact store stays for result bytes: it would need a
presigned read per question and suits the rich content this decision leaves out of scope.

**A question is an ordered list of prompts.** Each prompt has an `id`, a `header`, the question `text` and optional
`options` of `label` and `description`. At most 8 prompts, 2048 characters of text per prompt (counted as
JSON Schema counts them, in Unicode code points) and 16 KiB for the whole question; an adapter that exceeds them has its `attention.requested` entry rejected. The question travels in the
existing `attention.requested` evidence entry as an optional `question` member, so a request without one is valid and
an older adapter keeps working.

**An answer maps a prompt id to its text.** The decision command accepts `answer`, an object of prompt id to a list of
strings, alongside `answer_digest`. The control plane computes SHA-256 over the answer in its canonical JSON form
(RFC 8785) and refuses the decision unless it equals `answer_digest`, so the recorded digest is always the digest of
the delivered text. Every id must name a prompt of the question; a prompt without options accepts free text, one with
options accepts one of its labels unless the adapter marked it `other`. The decision record still holds the digest
only; the text sits in its own row.

**The task owner reads the question, as the connector proposal is read.**
`GET /v1/tenants/{tenant}/attention/{attention}/question` returns the question and the attention version to use as
`If-Match`. A principal that does not own the task gets 404, exactly as for a connector proposal. Tenant-wide read
permission still sees the attention's safe metadata but never the text. The issue says "responders"; the responder
set is the owner for a question, and widening it is a later decision once an attention can name other responders.

**The steer carries the answer's text to the runtime.** The decision delivery body gains the `answer` member for an
answer decision. The Codex adapter maps `item/tool/requestUserInput` to a question attention and, on an `answer`
decision, replies to Codex with the answers keyed by prompt id. It still refuses an answer for a native request that
is not a question.

**Secret prompts are not accepted here.** A prompt Codex marks `isSecret` is declined: the adapter answers the native
request with no answers, raises no attention and records no text. Storing a secret in a table and an outbox payload
would contradict the rule that secrets never reach the product database (ADR-0022); a delivered-once path is a
separate decision.

**Retention and redaction.** Question and answer text live as long as the task. They are not copied into audit
entries, logs, events or the outbox's inspectable evidence: those carry the digest, and the outbox row that delivers
an answer is cleared of its text once the delivery is settled. An expired or superseded request keeps its question and
never accepts an answer.

## Alternatives considered

- **Store the text in the artifact store:** rejected. It needs a presign and a fetch for a few hundred bytes, and
  makes the question flow depend on an optional role.
- **Keep the text in the runtime's own transcript and fetch it on demand:** rejected. A runtime that has stopped, or
  one that never keeps a transcript, would leave the surface with nothing, and the control plane would be reading
  another system's private state.
- **Serve the question to every principal with task read permission:** rejected. A question can quote the task's
  private content; the owner-only rule matches the connector proposal.
- **Persist secret answers and redact them later:** rejected for the reason under secret prompts.

## Consequences

- Two tables and one migration; two schema members (`question`, `answer`) and one read operation.
- Adapters built with kit v0.3.0 and v0.4.0 keep working: they send no question and get no answer text.
- The reference application can show a question and send an answer's text; its browser digest check becomes a
  consistency check, not the only record.
- A runtime that offers a secret prompt gets no answer from dac-plane until the delivered-once decision exists.
- The text is stored unencrypted at the application level, like other product records; the database's own protection
  applies.
