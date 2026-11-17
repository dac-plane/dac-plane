# GitHub App connector

This separate Go module implements the `connector` provider role using GitHub App
user authorization. It exposes `get_issue`, `create_comment` and
`list_repositories`; credentials remain in an AES-256-GCM sealed bbolt vault.

Build with `go build ./cmd/dac-github-adapter`, or use the root Docker target
`dac-github-adapter`. The image is buildable locally; it is not added to release
publication by this feature.

Configure the variables documented in
[`docs/guides/reference/configuration.md`](../../docs/guides/reference/configuration.md#dac-github-adapter).
Register the provider and bind its identity for the tenant's `connector` role.
Give this process a private durable volume for its vault and mount its 32-byte key
separately. The callback listener may run behind a TLS reverse proxy on loopback;
non-loopback listeners require their certificate and key.

Consent requests `offline_access`, sends a PKCE challenge and validates a
single-use, expiring state. The callback gives its browser a second confirmation
proof, in the fragment of the fixed surface return URL or in the callback's JSON
response. The owner-authenticated Surface API completion must send this proof.
Polling cannot obtain it. A GitHub token remains provisional until that proof is
presented. Renewals retain all previous credential versions until remote access
revocation is confirmed; removal then deletes their local refresh tokens.

The vault records a `sent` marker before a GitHub tool request. Losing a response
after that point leaves an uncertain outcome; restart and redelivery never repeat
the request. `read_call` reads only a recorded result. Cancellation before that
marker can be proven not sent, including recovery after a crash while preparing.

Run `just test-github-adapter` from the repository root for deterministic OAuth,
confirmation binding, encryption, refresh, revocation, projection, cancellation,
restart and unknown-outcome tests against a fake GitHub service.

## Live qualification

Run this only with a dedicated qualification GitHub App account and repository.
First authorize its connection through the Surface API and callback and complete
it using the browser proof. Stop the connector process while the qualification
runner owns the vault; bbolt permits one process at a time.

Export the adapter's normal configuration plus:

- `DAC_GITHUB_QUALIFICATION_CONNECTION`: that connection ID.
- `DAC_GITHUB_QUALIFICATION_OWNER` and `DAC_GITHUB_QUALIFICATION_REPO`: the fixture repository.
- `DAC_GITHUB_QUALIFICATION_ISSUE`: a fixture issue number.

`just qualify-github-connector` performs fresh read calls and checks replayed
results. Set `DAC_GITHUB_QUALIFICATION_WRITE=true` to add one comment to the fixture
issue and verify replay does not post a second comment. This is an explicit
external write. The runner prints test outcomes and never prints credentials or
unrestricted GitHub responses. Remove the fixture connection through the Surface
API afterwards to test remote revocation, then restart the connector to let the
dac-plane confirm removal.

This recipe qualifies the adapter's GitHub API behavior. Product gateway tests,
runtime behavior, and runtime-host egress containment are separate checks. The
live recipe is not part of deterministic CI.

GitHub's current behavior is documented in its
[App user authorization flow](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app)
and [OAuth token management API](https://docs.github.com/en/rest/apps/oauth-applications).
