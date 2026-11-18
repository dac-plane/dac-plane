# Serve a question's content and deliver the answer's text

Tracker: https://github.com/dac-plane/dac-plane/issues/157
Branch: `feat/157-attention-content-and-answer`, from `main` at `93078cf`.
Decision: ADR-0026 (to be written once the open decisions below are settled).
Needed by: the reference application (#97), whose question flow cannot complete without it.

## Where the tree stands (investigation, #157 "Investigate first")

| Finding | Evidence | Consequence |
|---|---|---|
| An attention request has no human-readable content | `lifecycle.AttentionRequest`; `contracts.AttentionRequested` carries kind, native ref, expiry, and for approvals the action and digest | A question's text has no home today. |
| An answer decision carries only `answer_digest` | `lifecycle/attention.go` "stored outside the decision record"; `application.DecideAttention` validates the digest and nothing else | No operation accepts the answer's text; the worker's steer body has the digest only. |
| The Codex adapter never raises a question | `adapters/codex/adapter.go` emits `attention.requested` with kind `action_approval` only; `steer` rejects every decision except `approve_once` and `deny` | The kind `question` and decision `answer` exist in the domain but no real runtime uses them. |
| Codex has a native question request | `codex app-server generate-json-schema --experimental` (0.169.0): server request `item/tool/requestUserInput`, params `questions[]` of `{id, header, question, options[]?, isOther, isSecret}` plus `isBlocking`; response `{answers: {<question id>: {answers: [string]}}}` | The adapter can map it to a question attention and answer it with text. It is marked EXPERIMENTAL; the adapter pins 0.165.0, so availability on the pin is unverified. |
| The connector proposal is the precedent | `application.GetConnectorProposal`: owner-only, 404 for anyone else, bound to the attention version | The question read follows the same rules. |
| The proposal's content lives in a product table | `store.ConnectorCall` keyed by operation | A product record is the existing pattern for content a surface must read back. |

## Decisions (accepted, ADR-0026)

1. **Storage.** Product records in PostgreSQL, one row per attention (question) and per decision (answer).
2. **Readers.** The task owner only, 404 otherwise, as the connector proposal. Widening to a responder set waits
   until an attention can name other responders.
3. **Secret prompts.** Declined at the adapter: the native request is answered with no answers, no attention is
   raised, no text is stored.
4. **Retention.** Text lives as long as the task, stays out of audit, logs and events, and the delivery outbox row
   is cleared of its text once settled.
5. **Shape.** An ordered list of prompts; answers keyed by prompt id, one string each; the digest is SHA-256 over
   the answer in RFC 8785 form. Limits count characters (code points), as the schemas do.

Codex 0.165.0, the pinned version, has `item/tool/requestUserInput` (read from its generated schema, not a live run).
A decision on a question that has stored text must carry the matching answer; one on a question without stored text
stays digest-only, as the reference application sends today.

## Stages

1. **Decision and contracts. Done (PR 167).** ADR-0026; `lifecycle.Question` and `lifecycle.Answer`; question and
   answer schemas; optional `question` on `attention.requested`; optional `answer` on the decision request. The kit
   needs no change: evidence entries are generic and both members are optional.
2. **Persistence. Done.** Migration 00019 (`attention_question`, `decision_answer`, append-only); store reads and writes
   with replay comparison; ingestion stores a question's text; a binary on schema 18 raises the question without text
   (ADR-0011). The owner-scoped read comes with the API stage.
3. **Application and Surface API.** `GET /attention/{attention}/question`, bound to the attention version; the
   decision accepts the answer text and checks it against `answer_digest`; OpenAPI and problems.
4. **Worker and adapters.** The steer carries the answer text; the Codex adapter maps
   `item/tool/requestUserInput` to a question attention and the answer back to its response; the simulator and
   conformance scenarios.
5. **Documentation and end-to-end.** Surface API guide, adapter pages; a check with the Codex adapter that the
   delivered text matches the recorded digest.

## Acceptance (from the issue)

- Accepted ADR on where content is kept and who reads it.
- A responder reads the question; a non-responder gets 404.
- A runtime receives the answer's text, shown end to end with the Codex adapter.
- The recorded digest matches the delivered content, shown by a test.
