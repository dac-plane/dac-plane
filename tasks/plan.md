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

## Open decisions (need an answer before the ADR)

1. **Where question and answer text live.** Recommended: product records in PostgreSQL, one row per attention
   (question) and per decision (answer), size-bounded, because both are short, must be read back by a surface and
   must be delivered by the worker. Alternative: artifact store (heavier, needs a presign path, suits rich content,
   which is out of scope).
2. **Who may read them.** Recommended: the task owner only, 404 otherwise, as the connector proposal. The issue
   says "responders"; responders are not always the owner, so the ADR must pick: owner only, or the attention's
   responder set.
3. **Secret questions.** Codex marks a question `isSecret`. Recommended: refuse to store a secret answer (answer is
   delivered once, never persisted, only its digest recorded) or decline `isSecret` questions in this issue.
4. **Retention and redaction.** Recommended: keep text for the attention's life plus the task's retention; redact
   from audit and logs (digest only, as the connector gateway keeps credentials out of frames).
5. **Multi-question requests.** Codex sends several questions per request. Recommended: carry them as an ordered
   list of `{id, header, text, options}` and take answers as a map by id, so the digest covers the canonical
   encoding of the whole answer.

## Stages (after the decisions)

1. **Decision and contracts.** ADR-0026; question content on `attention.requested`; answer content on the
   decision command; JSON Schemas, agreement tests, kit mirror, capability negotiation so kit v0.3.0 and v0.4.0
   adapters keep working.
2. **Persistence.** Migration for question and answer content; digest check on write; owner-scoped read.
3. **Application and Surface API.** `GET /attention/{attention}/question`, bound to the attention version; the
   decision body accepts the answer text and checks it against `answer_digest`; OpenAPI and problems.
4. **Worker and adapters.** Steer carries the answer text; the Codex adapter maps `item/tool/requestUserInput`
   to a question attention and the answer back to its response; the simulator and conformance scenarios.
5. **Deployment and documentation.** Surface API guide, adapter pages, configuration reference if touched; an
   end-to-end check with the Codex adapter that the delivered text matches the recorded digest.

## Acceptance (from the issue)

- Accepted ADR on where content is kept and who reads it.
- A responder reads the question; a non-responder gets 404.
- A runtime receives the answer's text, shown end to end with the Codex adapter.
- The recorded digest matches the delivered content, shown by a test.
