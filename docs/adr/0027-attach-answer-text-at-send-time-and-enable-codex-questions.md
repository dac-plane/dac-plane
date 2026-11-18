# ADR-0027: Attach an answer's text when the steer is sent, and enable questions in Codex threads

- Status: Accepted
- Date: 2026-11-18
- Issue: [#157](https://github.com/dac-plane/dac-plane/issues/157)
- Refines: [ADR-0026](0026-keep-question-and-answer-text-as-product-records.md)

## Context

[ADR-0026](0026-keep-question-and-answer-text-as-product-records.md) keeps a question's and an answer's text as
product records and says the outbox row that delivers an answer "is cleared of its text once the delivery is settled".
Building the delivery showed a stricter and simpler rule, and a live run against Codex app-server 0.165.0 and Camer's
`deepseek-v4-flash-0731` showed two facts the protocol schema does not:

- Codex answers the `request_user_input` tool with "unavailable in Default mode" unless the thread sets
  `features.default_mode_request_user_input`. The pinned binary ships the flag.
- In that mode Codex sends `isBlocking: false` with the request, yet it waits for the reply: raising the question and
  replying when it is answered ended with the model writing the typed answer.

## Decision

**The outbox never holds an answer's text.** The delivery row keeps the decision's digest only. When the worker sends
the steer it reads the text from the decision's own record and adds it to the arguments, so there is no text to clear
before or after the send, and an exhausted or poisoned delivery row cannot retain it. This replaces ADR-0026's
"cleared once settled" for the outbox; the text still lives as long as the task in the decision's record. The text
is encoded without HTML escaping, so the arguments are the size the answer's own bytes imply and a valid answer cannot
exceed the 64 KiB request bound because of `<`, `>` or `&`.

**The Codex adapter starts and resumes every thread with `features.default_mode_request_user_input`.** Without it the
model can never ask a person anything.

**The Codex adapter raises a question whatever `isBlocking` says.** It still declines a prompt marked secret and a
request outside the question bounds, as ADR-0026 decides.

## Consequences

- A worker that cannot read the decision's record cannot deliver an answer's text and retries the delivery.
- Every Codex thread carries one more feature flag. A Codex version that renames it would stop questions from being
  asked, and `just qualify-codex-question` is the check that finds out.
