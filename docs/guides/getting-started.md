# Getting started

This walkthrough runs dac-plane on your machine against simulated providers, then takes one task from submission to
an accepted result through the Surface API. It needs no provider account and takes about ten minutes, most of it
building the images.

The simulated runtime, environment and artifact store behave like real ones at the protocol level, but do no real
work. To run real agents, see [Deploy](deploy/index.md).

## Before you start

You need:

- Docker with Compose v2.24.4 or later. On macOS, Docker Desktop, OrbStack or Colima.
- [just](https://github.com/casey/just), `openssl`, `curl`, `jq` and `uuidgen`.
- A clone of the repository:

```bash
git clone https://github.com/dac-plane/dac-plane.git
cd dac-plane
```

## Start the stack

```bash
just stack-up
```

This generates a throwaway CA, certificates and secrets under `.work/stack/`, builds the images, and starts:

- PostgreSQL, and a one-shot job that applies the schema;
- a test OpenID Connect issuer;
- jobs that provision a tenant, an owner, and a runtime, an environment and an artifact store;
- `dac-plane serve`;
- the simulator, connected as all three providers.

The Surface API listens on `127.0.0.1:18443` and the issuer on `127.0.0.1:18080`. If either port is taken, export
`DAC_SURFACE_PORT` or `DAC_ISSUER_PORT` with a free port before `just stack-up`. Everything the stack generates is a
test fixture: never reuse its keys or issuer anywhere else.

Check that the control plane is ready:

```bash
docker compose -p dac-plane exec -T dac-plane /dac-plane health ready
```

## Get a token

Load the stack's settings into your shell. If you changed a port above, export it again after this line.

```bash
set -a; . .work/stack/compose.env; set +a
```

The stack provisioned fixed identifiers, so the commands below can name them:

```bash
tenant=01a167fd-51d9-76ba-b563-496e2bd89646
runtime=01a167fd-51d9-7d22-8a3b-6c5d4e3f2a10
environment=01a167fd-51d9-7d44-8a3b-6c5d4e3f2a12
base="https://dac-plane:8443/v1/tenants/$tenant"
```

The certificates name the hosts `issuer` and `dac-plane`, so `curl` connects to the published ports under those
names and trusts the stack's CA:

```bash
issuer=(--cacert "$DAC_CA_FILE" --connect-to "issuer:8443:127.0.0.1:$DAC_ISSUER_PORT")
api=(--cacert "$DAC_CA_FILE" --connect-to "dac-plane:8443:127.0.0.1:$DAC_SURFACE_PORT")
```

Ask the issuer for an access token as `surface-owner`, the client the stack bound to the tenant's owner:

```bash
token=$(curl -fsS "${issuer[@]}" https://issuer:8443/default/token \
  -u "surface-owner:$(cat "$DAC_STACK_SURFACE_CLIENT_SECRET_FILE")" \
  -d grant_type=client_credentials -d scope=dac-plane-surface | jq -r .access_token)
auth=(-H "Authorization: Bearer $token")
```

## Submit a task

A submission names the runtime and the environment to use, what done means, a deadline, and a budget: here, up to
1000 tokens, with 100 reserved to start.

Every command carries an `Idempotency-Key`. Choose the key and the deadline once, before the request, so the request
can be sent again unchanged:

```bash
key=$(uuidgen)
deadline=$(date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v+1H +%Y-%m-%dT%H:%M:%SZ)
```

```bash
task=$(curl -fsS "${api[@]}" "${auth[@]}" -X POST "$base/tasks" \
  -H "Idempotency-Key: $key" -H 'Content-Type: application/json' \
  -d '{
    "provider_id": "'"$runtime"'",
    "environment_provider_id": "'"$environment"'",
    "objective": "Write a short status report",
    "acceptance_criteria": ["The report is attached"],
    "demand": "person",
    "deadline": "'"$deadline"'",
    "unit": "tokens", "ceiling": 1000, "amount": 100
  }' | jq -r .task.task_id)
echo "$task"
```

dac-plane creates an attempt, the environment is provisioned and the runtime starts.

If the response is lost, run the request block again as it is. With the same key and the same body, dac-plane
returns the task it already created instead of creating a second one. A new key is a new submission, and the same
key with a different body is refused with `422`.

## Answer the attention request

The simulated agent stops to ask a question, as a real one would before running a command it needs permission for.
Wait for its attention request to appear:

```bash
until attention=$(curl -fsS "${api[@]}" "${auth[@]}" "$base/attention" |
  jq -ce --arg task "$task" 'first(.attention[] | select(.task_id == $task)) // empty'); do sleep 1; done
jq '{attention_id, state, version}' <<<"$attention"
```

Answer it. dac-plane records the SHA-256 digest of the answer. `If-Match` carries the version of the request you
are answering, so an answer to a request that changed in the meantime is refused rather than applied to the wrong
version.

```bash
answer_digest="sha256:$(printf 'Yes, go ahead.' | shasum -a 256 | cut -d' ' -f1)"
curl -fsS "${api[@]}" "${auth[@]}" -X POST "$base/attention/$(jq -r .attention_id <<<"$attention")/decisions" \
  -H "Idempotency-Key: $(uuidgen)" -H "If-Match: \"$(jq -r .version <<<"$attention")\"" \
  -H 'Content-Type: application/json' \
  -d '{"kind": "answer", "answer_digest": "'"$answer_digest"'"}' | jq -c '{kind, state}'
```

## Review the result

The agent continues, commits its output to the artifact store, and proposes a result. Wait until the task is ready
for review:

```bash
for _ in $(seq 60); do
  [ "$(curl -fsS "${api[@]}" "${auth[@]}" "$base/tasks/$task" | jq -r .task.state)" = ready_for_review ] && break
  sleep 1
done
curl -fsS "${api[@]}" "${auth[@]}" "$base/tasks/$task" |
  jq '{state: .task.state, revision: .revisions[0].revision.revision_id, artifacts: .revisions[0].revision.artifact_ids}'
```

Accept the revision. The task's `ETag` goes in `If-Match`, so the review applies only to the version you read.

```bash
etag=$(curl -fsS -D - -o /dev/null "${api[@]}" "${auth[@]}" "$base/tasks/$task" | tr -d '\r' |
  awk -F': ' 'tolower($1) == "etag" {print $2}')
revision=$(curl -fsS "${api[@]}" "${auth[@]}" "$base/tasks/$task" | jq -r '.revisions[0].revision.revision_id')
curl -fsS "${api[@]}" "${auth[@]}" -X POST "$base/tasks/$task/reviews" \
  -H "Idempotency-Key: $(uuidgen)" -H "If-Match: $etag" -H 'Content-Type: application/json' \
  -d '{"revision_id": "'"$revision"'", "disposition": "accept"}' | jq -c '{review: .review.disposition}'
```

The task ends `succeeded`:

```bash
sleep 2
curl -fsS "${api[@]}" "${auth[@]}" "$base/tasks/$task" | jq -r .task.state
```

## Read what happened

Every change to the record is an event. Read the task's events as a page:

```bash
curl -fsS "${api[@]}" "${auth[@]}" "$base/tasks/$task/events" |
  jq -r '.events[] | "\(.record_kind) \(.event): \(.from_state) -> \(.to_state)"'
```

The list shows the whole run: the submission and budget reservation, the environment provision and runtime start,
the wait for input and your decision, the artifact commit, verification and your review. Send
`Accept: text/event-stream` to follow the same events live instead; each event's `cursor` lets a client resume where
it stopped.

## Stop the stack

```bash
just stack-down
```

## Next

- [Concepts](concepts.md) explains the records you just created.
- [Use the Surface API](use/surface-api.md) covers the rest of the API, and the
  [API reference](reference/api.html) lists every operation.
- [Deploy](deploy/index.md) runs dac-plane with real providers.
