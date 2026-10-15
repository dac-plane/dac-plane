# Deep dives 04 — lifecycle message fields and the hook contract

Sources: `evidence/extracted-beautified/` (line-cited). All protobuf field numbers are
from the embedded `static $()` definitions — the wire contract is recoverable in full.

## 1. Lifecycle messages (`aiserver.v1`) — contract vocabulary

`dist/electron-main/proto.cjs:43926` and coordinator, selected messages:

### Transcript (the progress/event log)

- `GrokBotTranscriptEntry {seq, entry_kind, body(bytes)?, blob_hash?, updated_seq,
  entry_id?, body_omitted?}` — entries are identified by `seq`/`updated_seq` pairs;
  big bodies live behind `blob_hash` (`body_omitted` flag). `entry_kind` is a string;
  observed values in code: `user_message`, `assistant_text`, `tool_call`, `thinking`.
- `GrokBotTranscriptDelete {seq, updated_seq, entry_id?}` — tombstones are part of
  the log (revisions delete, they do not rewrite).
- `GrokBotTranscriptCursor {agent_id, generation, after_updated_seq, session_id}` —
  the rehydration cursor.
- `GrokBotTranscriptWatchFrame` oneof (the server stream): `connected{stream_id,
  server_time_ms, absolute_lifetime_ms}` → `rows{entries, deletes, replay flag}` /
  `cleared{new_generation}` / `cursor_too_old` / `heartbeat{server_time_ms}` /
  `agent_state{live[], snapshot?}` / `agent_state_changed{agent_id, families[],
  changed_at_ms}` / `turn_failed{agent_id, session_id, turn_id, code, summary,
  failed_at_ms, error_details?, account}` / `roster_changed{kind, agent_id, agent?,
  changed_fields[]}` / `box_state{state, snapshot?}` / `computer_actions` /
  `page_title_changed`. A complete, dedupable event stream with
  cursor-too-old → rehydrate semantics.

### Identity and sessions

- `GrokBotAgent {id, legacy_agent_id, name, description, title, avatar_*, created/
  updated_at_ms, harness, role, visibility?, team_id?, viewer_* (is_owner,
  can_manage, session_id), kind (AGENT/ROOM), member_agent_ids[], people[],
  published_at?, team_default_for_viewer, voice_*, drafting_disabled}` — one agent
  record carries access, presence and voice in one message.
- `GrokBotAgentSession {agent_id, session_id, kind, created/updated/
  last_activity_at_ms}`; `GrokBotSessionBox {agent_id, session_id, auth_id,
  tenant_id, store_id, provisioned_at_ms, last_activity_at_ms, reap_c...}` —
  the session↔environment binding is a first-class record (agent+session → box).
- `GrokBotSessionBoxPod {cluster, pod_id, phase, creation/deletion_timestamp_ms,
  node_id...}`.

### Turns and interruption

- `TurnFailed` code enum + summary + error_details + account (see watch frame).
- `InterruptGrokBotAgentRunRequest {agent_id, reason, session_id?}` →
  `InterruptGrokBotAgentRunResponse {had_active_run}` — interruption acknowledges
  whether a run was actually active.
- `NotifySandAgentTurnFinishedRequest {agent_id, agent_name, message_preview,
  last_message_id, awaiting_user_response, member_agent_ids[], sender_agent_...}` —
  turn end reports whether it awaits the user (the NEEDS_INPUT transition).
- Room member turns (`RequestGrokBotRoomMemberTurnRequest {nonce, room, peers[],
  new_messages[], is_winding_down, deadline_ms, parent_request_id?}` → response with
  `dispatch` + `workflow_id?`; `DeliverGrokBotRoomMemberTurnResultRequest {room_id,
  nonce, member_agent_id, outcome, messages[], error, posts[]}`) — multi-agent rooms
  turn-take with nonces, deadlines and workflow-backed dispatch.

### Spend ceremony (two-phase, full contract)

- `BeginGrokBotSpendApprovalRequest {agent_id, request_id, payment_method_id?,
  mode (ASSERT/REGISTER), enrollment_ticket?}` →
  `BeginGrokBotSpendApprovalResponse {ceremony_id, challenge_id,
  request_options_json, creation_options_json, ceremony_path, ceremony_token}`
  (WebAuthn create/get options bundled) →
  `FinishGrokBotSpendApprovalAssertionRequest {ceremony_token, challenge_id,
  assertion_json, registration?}` → `...Response {credential_id}`.
- `SpendSummary {amount_cents, currency, merchant_name, merchant_url, context}` —
  what the approval dialog shows. `SpendApprovalAttestation {challenge_id}`,
  `SpendApprovalRegistration {attestation_json, name}`.
- `ConfigureSpendLimitAction {confirm_label}`.

## 2. Hook contract — the enforcement surface in detail

`dist/local-exec-daemon/main.cjs` (`ExecuteHookArgs {request}` →
`ExecuteHookResult {response}`; `ExecuteHookRequest`/`ExecuteHookResponse` oneof,
10 hook kinds: `pre_compact`, `subagent_start`, `subagent_stop`, `pre_tool_use`,
`post_tool_use`, `post_tool_use_failure`, `before_submit_prompt`,
`after_agent_response`, `after_agent_thought`, `stop`).

Every hook request carries **conversation_id + generation_id + model + model_params**
— hooks are traceable to the exact run. Selected shapes:

| Hook | Request (query) | Response capabilities |
|---|---|---|
| `pre_tool_use` | tool_name, tool_input, tool_use_id, cwd?, parent_tool_call_id? | `permission`, `user_message`, `agent_message`, **`updated_input`**, `additional_context` |
| `post_tool_use` | tool_name, tool_input, tool_output, duration_ms, tool_use_id | `additional_context` |
| `post_tool_use_failure` | tool_name, tool_input, error_message, failure_type, duration_ms, is_interrupt | `additional_context` |
| `before_submit_prompt` | prompt, attachments[] (type, file_path), composer_mode | `continue?`, `user_message`, `additional_context` |
| `stop` | status, loop_count, token counts (input/output/cache_read/cache_write) | `followup_message` |
| `subagent_start` | subagent_id, subagent_type, task, parent_conversation_id, is_parallel_worker, git_branch | `permission`, `user_message`, `additional_context` |
| `subagent_stop` | subagent_id, status, duration_ms, summary, message_count, tool_call_count, modified_files[], error_message | `followup_message`, `additional_context` |
| `pre_compact` | trigger, context_usage_percent, context_tokens, context_window_size, message_count, is_first_compaction | `user_message` |
| `after_agent_response` / `after_agent_thought` | text, duration_ms?, token counts | (context feeding) |

Key properties:

- Hooks **decide permission and can rewrite tool input** (`updated_input`) — the
  interception point is complete, not advisory-only.
- Token accounting flows through hooks (cache read/write too) — budget metering rides
  the same channel.
- `ShellHookApprovalRequirement {kind, reason?}` and `ShellStreamHookContext
  {hook_additional_contexts[]}` tie shell streaming to hook approvals;
  `HooksConfigInfo {configured_steps[]}` reports which hooks are configured.
- Hook `additional_context` carriers are drained into the caller's collector
  (metric label `hook_event_name`).

Tool-call envelope for comparison: `ExecServerMessage` (68755) oneofs every tool arg
(30+ tool families with field numbers 2-54) plus `span_context?` and
`accept_hook_additional_contexts` — the same envelope serves exec, MCP, computer-use,
subagents and prechecks.

## 3. dac-plane implications (delta to MASTER-RECORD §6)

- The **event stream contract** is directly adoptable for the runtime port's
  event leg: `{connected → rows|cleared|cursor_too_old|heartbeat}` plus
  tombstone deletes and `replay` flags, with entries as `{seq, updated_seq,
  blob_hash, body_omitted}` — payload bytes by hash, not inline. This is what
  "authenticated normalized-event ingestion" looks like in production.
- The **hook response schema** (permission + updated_input + messages + context)
  is the minimal complete interception contract — recommend mirroring it in the
  simulator's runtime hook before any real gateway integration.
- **Turn-failure carries account + error_details + code** — failure events are
  first-class records, matching dac-plane's AuditEvent/ResultRevision instincts.
- **Session↔environment binding record** (`GrokBotSessionBox`: agent+session →
  store/tenant/pod) — the correlation record between Attempt and environment that
  ownership rule 3 requires, shown concretely.
