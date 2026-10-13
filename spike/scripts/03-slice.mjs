#!/usr/bin/env node
// Thin slice + Q2-Q5 against the REAL native OpenClaw Gateway of a deployed OCC Agent.
// Product task -> native session/turn (chat.send) -> exec.approval.requested -> exec.approval.resolve.
// Every native event is recorded (spike/out/slice-events.jsonl); the product<->native ID mapping is
// persisted to spike/out/slice-mapping.json (stand-in for product.occ_binding / native_turn / attention_request).
// Usage: node 03-slice.mjs <q1-record.json> <scenario...>   scenarios: smoke approve deny cancel stale history
// Env: OCE_DIR (for @openclaw/gateway-client), GATEWAY_ORIGIN (default wss://127.0.0.1:8443),
//      OCC_SERVICE_KEY_FILE. TLS of the local dev Envoy is self-signed: NODE_TLS_REJECT_UNAUTHORIZED=0 (local spike only).
import { readFileSync, writeFileSync, appendFileSync, mkdirSync, existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { createRequire } from "node:module";
import { randomUUID } from "node:crypto";

const here = dirname(fileURLToPath(import.meta.url));
const out = join(here, "..", "out"); mkdirSync(out, { recursive: true });
const [recordPath, ...scenarios] = process.argv.slice(2);
const q1 = JSON.parse(readFileSync(recordPath, "utf8"));
const keyFile = process.env.OCC_SERVICE_KEY_FILE ?? "/tmp/openclaw-development/initial-admin-service-key.json";
const serviceKey = JSON.parse(readFileSync(keyFile, "utf8")).key;
const req = createRequire(join(process.env.OCE_DIR, "apps/controller/package.json"));
const { GatewayClient } = await import(pathToFileURL(req.resolve("@openclaw/gateway-client")).href);
const url = `${process.env.GATEWAY_ORIGIN ?? "wss://127.0.0.1:8443"}/namespaces/${q1.namespaceId}/agents/${q1.agentId}`;
const eventsFile = join(out, "slice-events.jsonl");
const mapFile = join(out, "slice-mapping.json");
const mapping = existsSync(mapFile) ? JSON.parse(readFileSync(mapFile, "utf8")) : { binding: { agentId: q1.agentId, namespaceId: q1.namespaceId, revisionId: q1.revisionId }, tasks: {} };
const saveMap = () => writeFileSync(mapFile, JSON.stringify(mapping, null, 2));
const now = () => new Date().toISOString();
const log = (m, o) => console.log(`[${now()}] ${m}${o ? " " + JSON.stringify(o) : ""}`);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const waiters = [];
function onEvent(evt) {
  const s = JSON.stringify(evt.payload ?? null);
  appendFileSync(eventsFile, JSON.stringify({ at: now(), event: evt.event, seq: evt.seq, payload: s.length > 4000 ? s.slice(0, 4000) + "…" : JSON.parse(s) }) + "\n");
  for (const w of [...waiters]) if (w.match(evt)) { waiters.splice(waiters.indexOf(w), 1); w.resolve(evt); }
}
const waitEvent = (match, ms) => new Promise((resolve, reject) => {
  const w = { match, resolve }; waiters.push(w);
  setTimeout(() => { const i = waiters.indexOf(w); if (i >= 0) { waiters.splice(i, 1); reject(new Error("event timeout")); } }, ms);
});

let hello;
const client = new GatewayClient({
  url, clientName: "gateway-client", mode: "backend", role: "operator", scopes: [], caps: ["approvals"],
  deviceIdentity: null, edgeAuthHeaders: { "x-api-key": serviceKey }, onEvent,
  onHelloOk: (h) => { hello = h; }, onConnectError: (e) => log("connect.error", { message: String(e?.message ?? e) }),
});
client.start();
for (let i = 0; i < 60 && !hello; i++) await sleep(500);
if (!hello) { log("FAIL gateway hello timeout"); process.exit(1); }
log("gateway.hello", { role: hello.auth?.role, scopes: hello.auth?.scopes, server: hello.server?.version ?? hello.server });
mapping.binding.gatewayUrl = url; mapping.binding.helloScopes = hello.auth?.scopes; saveMap();

const call = async (method, params, timeoutMs = 30000) => {
  try { const r = await client.request(method, params, { timeoutMs }); log(`rpc.${method}.ok`, r && typeof r === "object" ? Object.fromEntries(Object.entries(r).slice(0, 8)) : r); return { ok: true, r }; }
  catch (e) { const err = { message: e?.message, code: e?.code ?? e?.gatewayCode, details: e?.details }; log(`rpc.${method}.error`, err); return { ok: false, err }; }
};
const text = (m) => typeof m.content === "string" ? m.content : (m.content ?? []).map((p) => p?.text ?? (p?.type ? `[${p.type}]` : "")).join(" ");
async function history(sessionKey) { const h = await call("chat.history", { sessionKey, limit: 50 }, 15000); return h.ok ? (h.r.messages ?? []) : []; }
async function waitAssistant(sessionKey, pred, ms) {
  const end = Date.now() + ms;
  while (Date.now() < end) { const msgs = await history(sessionKey); const hit = msgs.filter((m) => m.role === "assistant").find((m) => pred(text(m), m)); if (hit) return hit; await sleep(3000); }
  return null;
}
function task(kind) {
  const id = `task_${kind}_${Date.now()}`; const sessionKey = `agent:main:spike-${kind}-${Date.now()}`;
  mapping.tasks[id] = { kind, sessionKey, createdAt: now(), attention: [] }; saveMap(); return { id, sessionKey, t: mapping.tasks[id] };
}
async function send(t, sessionKey, message) {
  const ack = await call("chat.send", { sessionKey, idempotencyKey: randomUUID(), message }, 30000);
  t.runId = ack.r?.runId; t.sendAck = ack.ok ? "ok" : ack.err; saveMap(); return ack;
}
const execPrompt = (cmd) => `Use your shell/exec tool to run exactly this command and nothing else: \`${cmd}\`. Then reply with the command output, or say DENIED if it was not allowed to run.`;
async function approvalFor(t, sessionKey, ms = 240000) {
  try {
    const evt = await waitEvent((e) => e.event === "exec.approval.requested" && (!e.payload?.request?.sessionKey || e.payload.request.sessionKey === sessionKey || String(e.payload.request.sessionKey).endsWith(sessionKey.split(":").pop())), ms);
    const a = { approvalId: evt.payload.id, command: evt.payload.request?.command ?? evt.payload.request?.rawCommand, nativeSessionKey: evt.payload.request?.sessionKey, runId: evt.payload.request?.runId, requestedAt: now() };
    t.attention.push(a); saveMap(); log("attention.raised", a); return a;
  } catch { t.attention.push({ none: true, note: "no exec.approval.requested within timeout" }); saveMap(); log("attention.none"); return null; }
}
const results = {};
for (const s of scenarios) {
  log(`=== scenario ${s}`);
  if (s === "smoke") {
    const { sessionKey, t } = task("smoke"); const marker = `SPIKE-OK-${Date.now() % 100000}`;
    await send(t, sessionKey, `Reply with exactly ${marker} and nothing else.`);
    const m = await waitAssistant(sessionKey, (x) => x.includes(marker), 180000);
    t.outcome = m ? "completed" : "no_marker"; t.stopReason = m?.stopReason; saveMap(); results.smoke = t.outcome;
  }
  if (s === "approve") {
    const { sessionKey, t } = task("approve"); const marker = `SPIKE-APPROVED-${Date.now() % 100000}`;
    await send(t, sessionKey, execPrompt(`echo ${marker}`));
    const a = await approvalFor(t, sessionKey);
    if (a) { const r = await call("exec.approval.resolve", { id: a.approvalId, decision: "allow-once" }); a.decision = "allow-once"; a.resolveOk = r.ok; }
    const m = await waitAssistant(sessionKey, (x) => x.includes(marker), 240000);
    t.outcome = !a ? "no_attention" : m ? "completed_after_approval" : "no_result"; saveMap(); results.approve = t.outcome;
  }
  if (s === "deny") {
    const { sessionKey, t } = task("deny"); const f = `/tmp/spike-deny-${Date.now()}`; t.sideEffectFile = f;
    await send(t, sessionKey, execPrompt(`touch ${f} && echo CREATED`));
    const a = await approvalFor(t, sessionKey);
    if (a) { const r = await call("exec.approval.resolve", { id: a.approvalId, decision: "deny" }); a.decision = "deny"; a.resolveOk = r.ok;
      const again = await call("exec.approval.resolve", { id: a.approvalId, decision: "allow-once" }); a.secondResolveAfterDeny = again.ok ? "ACCEPTED" : again.err; }
    const m = await waitAssistant(sessionKey, () => true, 180000);
    t.outcome = a ? "denied" : "no_attention"; t.finalAssistant = m ? text(m).slice(0, 300) : null; saveMap(); results.deny = t.outcome;
  }
  if (s === "cancel") {
    const { sessionKey, t } = task("cancel"); const f = `/tmp/spike-cancel-${Date.now()}`; t.sideEffectFile = f;
    await send(t, sessionKey, execPrompt(`sleep 90 && touch ${f}`));
    const a = await approvalFor(t, sessionKey);
    if (a) { const r = await call("exec.approval.resolve", { id: a.approvalId, decision: "allow-once" }); a.decision = "allow-once"; a.resolveOk = r.ok; }
    await sleep(10000);
    const ab = await call("chat.abort", { sessionKey, ...(t.runId ? { runId: t.runId } : {}) }); t.abort = ab.ok ? ab.r : ab.err; t.abortAt = now();
    t.outcome = a ? "abort_requested_while_command_running" : "no_attention"; saveMap(); results.cancel = t.outcome;
  }
  if (s === "stale") {
    const { sessionKey, t } = task("stale"); const f = `/tmp/spike-stale-${Date.now()}`; t.sideEffectFile = f;
    await send(t, sessionKey, execPrompt(`touch ${f} && echo CREATED`));
    const a = await approvalFor(t, sessionKey);
    const ab = await call("chat.abort", { sessionKey, ...(t.runId ? { runId: t.runId } : {}) }); t.abort = ab.ok ? ab.r : ab.err;
    await sleep(5000);
    if (a) { const r = await call("exec.approval.resolve", { id: a.approvalId, decision: "allow-once" }); a.lateResolve = r.ok ? "ACCEPTED" : r.err; }
    await sleep(20000);
    t.outcome = a ? "late_approval_attempted" : "no_attention"; saveMap(); results.stale = t.outcome;
  }
  if (s === "history") {
    for (const [id, t] of Object.entries(mapping.tasks)) { const msgs = await history(t.sessionKey); t.historyAfterRestart = { messages: msgs.length, at: now() }; log("history", { id, messages: msgs.length }); }
    saveMap(); results.history = "read";
  }
}
log("RESULTS", results);
client.stop(); await client.stopAndWait?.({ timeoutMs: 1000 }).catch(() => {}); process.exit(0);
