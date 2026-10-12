#!/usr/bin/env node
// Q1: create Secret + Configuration + dedicated-Codex Agent through the OCC public API, grant the Agent
// `operate` on its exact Secret via IAM API (not DB writes), provision runtime credentials, deploy, and
// observe the deployment until it succeeds or fails. Records IDs and image pins to spike/out/q1-run.json.
//
// Model credential: read from OPENAI_API_KEY_FILE (preferred) or OPENAI_API_KEY. Never logged or written.
// --negative-fixture uses an obviously invalid value to observe the fail-closed path (no real key needed).
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const out = join(here, "..", "out");
mkdirSync(out, { recursive: true });
const args = process.argv.slice(2);
const negative = args.includes("--negative-fixture");
const name = args.find((a) => !a.startsWith("--")) ?? (negative ? "spike-codex-negative" : "spike-codex");
const model = process.env.SPIKE_MODEL ?? "gpt-6-astra";
const origin = process.env.OCC_URL ?? "http://127.0.0.1:3000";
const keyFile = process.env.OCC_SERVICE_KEY_FILE ?? "/tmp/openclaw-development/initial-admin-service-key.json";
const serviceKey = JSON.parse(readFileSync(keyFile, "utf8")).key;

function modelCredential() {
  if (negative) return "invalid-negative-fixture-not-a-credential";
  const f = process.env.OPENAI_API_KEY_FILE, e = process.env.OPENAI_API_KEY;
  if (f && e) throw new Error("Set only one of OPENAI_API_KEY_FILE / OPENAI_API_KEY");
  const v = f ? readFileSync(f, "utf8").trim() : e;
  if (!v) throw new Error("NO_CREDENTIAL: set OPENAI_API_KEY_FILE (preferred) or OPENAI_API_KEY; or pass --negative-fixture");
  return v;
}

const log = (m, o) => console.log(`[${new Date().toISOString()}] ${m}${o ? " " + JSON.stringify(o) : ""}`);
async function api(method, path, body, okStatuses = [200, 201, 202]) {
  const r = await fetch(new URL(path, origin), {
    method, headers: { "x-api-key": serviceKey, ...(body ? { "content-type": "application/json" } : {}) },
    body: body ? JSON.stringify(body) : undefined, signal: AbortSignal.timeout(30_000),
  });
  const text = await r.text();
  let j; try { j = JSON.parse(text); } catch { j = { raw: text.slice(0, 500) }; }
  if (!okStatuses.includes(r.status)) {
    const e = new Error(`${method} ${path} -> ${r.status} ${j?.error?.code ?? ""} ${j?.error?.message ?? ""}`);
    e.status = r.status; e.body = j; throw e;
  }
  return j.data ?? j;
}

// Dedicated Codex native configuration, derived from OCE scripts/first-agent.mjs (pinned commit).
// Spike change: Codex app-server approvalPolicy "on-request" with workspace-write sandbox so that
// command execution can raise native approval requests (Q2). Unverified until a real turn runs.
function configuration() {
  const selected = `codex/${model}`;
  return {
    kind: "agent",
    values: {
      gateway: {
        mode: "local", bind: "lan", controlUi: { enabled: false },
        auth: { password: { source: "env", provider: "default", id: "OPENCLAW_GATEWAY_PASSWORD" } },
        http: { endpoints: { chatCompletions: { enabled: true } } },
      },
      agents: { defaults: { model: selected, skipBootstrap: true, models: { [selected]: { agentRuntime: { id: "codex" } } } } },
      models: { providers: { codex: { baseUrl: "http://127.0.0.1:9", api: "openai-responses", models: [{ id: model, name: model }] } } },
      plugins: { allow: ["codex"], entries: { codex: { enabled: true, config: { appServer: {
        mode: "guardian", approvalPolicy: "on-request", sandbox: process.env.SPIKE_CODEX_SANDBOX ?? "read-only",
        transport: "websocket", url: "${APP_SERVER_URL}", authToken: "${APP_SERVER_TOKEN}",
      } } } } },
    },
  };
}

const record = { startedAt: new Date().toISOString(), name, model, negativeFixture: negative, steps: [] };
const step = (s, d) => { record.steps.push({ at: new Date().toISOString(), step: s, ...d }); log(s, d); };
const save = () => writeFileSync(join(out, `q1-${name}.json`), JSON.stringify(record, null, 2));

try {
  const installation = await api("GET", "/installation");
  record.installationId = installation.id;
  const nss = await api("GET", "/namespaces");
  const ns = (nss.items ?? nss).find((n) => n.name === "default");
  if (!ns || ns.status !== "ready") throw new Error(`default Namespace not ready: ${JSON.stringify(ns)}`);
  record.namespaceId = ns.id; step("namespace.ready", { id: ns.id });
  const base = `/namespaces/${ns.id}`;

  const secret = await api("POST", `${base}/secrets`, { name: `${name}-model-key`, value: modelCredential() });
  record.secretId = secret.id; step("secret.created", { id: secret.id });
  const cfg = await api("POST", `${base}/configurations`, configuration());
  record.configurationId = cfg.id; step("configuration.created", { id: cfg.id, generation: cfg.generation });
  const agent = await api("POST", `${base}/agents`, {
    name, configurationId: cfg.id, executionMode: "dedicated",
    harnessAuth: { method: "api_key", source: { kind: "secret", namespaceId: ns.id, id: secret.id } },
  });
  record.agentId = agent.id; record.agentServicePrincipalId = agent.servicePrincipalId;
  step("agent.created", { id: agent.id, executionMode: agent.executionMode, servicePrincipalId: agent.servicePrincipalId });

  const role = await api("POST", `${base}/iam/roles`, { name: `${name} secret operate`, permissions: [{ action: "operate", resourceKind: "secret" }] });
  const binding = await api("POST", `${base}/iam/access-bindings`, {
    resourceKind: "secret", resourceId: secret.id, roleId: role.id, subjectKind: "identity", subjectId: agent.servicePrincipalId,
  });
  record.roleId = role.id; record.bindingId = binding.id; step("iam.secret_operate.granted", { roleId: role.id, bindingId: binding.id });

  const creds = await api("GET", `${base}/agents/${agent.id}/runtime-credentials`);
  if (!creds.transportConfigured) await api("POST", `${base}/agents/${agent.id}/runtime-credentials`, {});
  step("runtime_credentials.provisioned");

  const rev = await api("POST", `${base}/agents/${agent.id}/deploy`);
  record.revisionId = rev.id; step("deploy.requested", { revisionId: rev.id, revision: rev.revision });
  save();

  const deadline = Date.now() + Number(process.env.SPIKE_DEPLOY_TIMEOUT_MS ?? 20 * 60_000);
  let last;
  while (Date.now() < deadline) {
    const d = await api("GET", `${base}/agents/${agent.id}/deployments/${rev.id}`);
    const summary = { status: d.status, phase: d.phase, error: d.error?.code, cause: d.error?.causeKind ?? d.cause?.kind };
    if (JSON.stringify(summary) !== JSON.stringify(last)) { step("deployment.observed", summary); last = summary; }
    if (d.status === "succeeded" || d.status === "failed") { record.deployment = d; break; }
    await new Promise((r) => setTimeout(r, 10_000));
  }
  try { record.runtime = await api("GET", `${base}/agents/${agent.id}/deployments/${rev.id}/runtime`); } catch (e) { record.runtimeError = e.message; }
  try { record.runtimeImages = await api("GET", `${base}/agents/${agent.id}/runtime-images`); } catch (e) { record.runtimeImagesError = e.message; }
  try { record.agentAfter = await api("GET", `${base}/agents/${agent.id}`); } catch {}
  step("done", { deploymentStatus: record.deployment?.status ?? "timeout", activeRevisionId: record.agentAfter?.activeRevisionId });
} catch (e) {
  record.error = { message: e.message, status: e.status, body: e.body };
  step("error", { message: e.message });
  process.exitCode = 1;
} finally { save(); }
