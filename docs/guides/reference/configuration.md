# Configuration reference

Every dac-plane process is configured through environment variables. Secrets never go in a variable's value: a
variable ending in `_FILE` names a file that holds the secret, so it can come from the platform's secret mechanism,
such as a Compose or Kubernetes secret mounted as a file.

This page lists every variable each released binary reads. A test compares it with the source on every pull request,
so a variable added to a binary without a row here, or a row for a variable no binary reads, fails the build.

Durations use Go syntax, such as `90s`, `15m` or `1h`.

## dac-plane

The control plane. `serve` needs everything marked required; `migrate`, `health` and `provision` read only what they
use.

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `DAC_DATABASE_URL` | yes | | PostgreSQL connection URL. Set this or `DAC_DATABASE_URL_FILE`, which names a file holding it, not both. |
| `DAC_DATABASE_CONNECT_TIMEOUT` | no | `10s` | How long to wait for PostgreSQL at startup. A positive duration. |
| `DAC_IDENTITY_CONFIG_FILE` | to serve | | Path of the [identity configuration file](#identity-configuration-file): the issuers whose tokens are accepted. |
| `DAC_TLS_CERT_FILE` | to serve | | PEM certificate chain served by the Surface API, the adapter endpoint and a non-loopback operations listener. |
| `DAC_TLS_KEY_FILE` | to serve | | PEM private key for `DAC_TLS_CERT_FILE`. |
| `DAC_SURFACE_ADDR` | no | `:8443` | Listen address of the Surface API, as `host:port`. |
| `DAC_ADAPTER_ADDR` | no | `:9443` | Listen address of the adapter endpoint, as `host:port`. |
| `DAC_OPS_ADDR` | no | `127.0.0.1:8081` | Listen address of the operations listener (health and readiness). On loopback it serves plain HTTP; on any other address it serves TLS. |
| `DAC_DEPLOYMENT` | no | `production` | `production` or `development`. Production refuses an identity configuration that trusts a development issuer. |

## Every adapter

`dac-codex-adapter`, `dac-e2b-adapter` and `dac-s3-adapter` all read these. An adapter authenticates with either
`DAC_ADAPTER_TOKEN_FILE` or the four client-credentials variables.

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `DAC_ADAPTER_URL` | yes | | The control plane's adapter endpoint, such as `wss://dac-plane:9443`. |
| `DAC_PROVIDER_ID` | yes | | The provider identifier registered with `dac-plane provision register-provider`. |
| `DAC_ADAPTER_CA_FILE` | no | system roots | PEM bundle of the roots that sign the control plane's and the token endpoint's certificates. When set, it replaces the system roots. |
| `DAC_ADAPTER_TOKEN_FILE` | one way | | File holding a bearer token. It is read again for every connection, so something else can refresh it. |
| `DAC_ADAPTER_TOKEN_URL` | other way | | The identity provider's OAuth 2.0 token endpoint, for the client-credentials grant. |
| `DAC_ADAPTER_CLIENT_ID` | other way | | The adapter's client ID at that identity provider. |
| `DAC_ADAPTER_CLIENT_SECRET_FILE` | other way | | File holding the adapter's client secret. |
| `DAC_ADAPTER_SCOPE` | other way | | Scope requested with the token. Configure the identity provider so this scope yields a token for the adapter endpoint's audience. |

## dac-codex-adapter

The agent runtime adapter for Codex. It runs a pinned Codex app-server.

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `DAC_CODEX_APP_SERVER` | yes | | Absolute path of the Codex app-server binary. Its version must match the one the adapter was built for. |
| `DAC_CODEX_HOME` | yes | | Persistent directory used as the app-server's home, isolated from any user's. |
| `DAC_CODEX_STATE` | yes | | Path of the adapter's persistent journal file. |
| `DAC_CODEX_WORKSPACE` | yes | | Absolute path of the workspace the agent works in. With `DAC_CODEX_EXEC_SERVER_URL` set, a path inside the environment. |
| `DAC_CODEX_MODEL` | yes | | The model the agent uses. |
| `DAC_CODEX_MODEL_PROVIDER` | yes | | Name of the model provider in the Codex configuration. |
| `DAC_CODEX_BASE_URL` | yes | | HTTPS base URL of the model provider's Responses API. |
| `DAC_CODEX_API_KEY_FILE` | yes | | File holding the model provider's API key. |
| `DAC_CODEX_EXEC_SERVER_URL` | with the next | | Runs agent commands inside the environment: the exec-server address of a provisioned resource, with `{resource}` standing for its identifier. Set it together with `DAC_ENVIRONMENT_ATTACH_KEY_FILE`, or neither. |
| `DAC_ENVIRONMENT_ATTACH_KEY_FILE` | with the previous | | File holding the key shared with the environment adapter, which lets this adapter attach to the resource's exec-server. |

## dac-e2b-adapter

The environment adapter for E2B sandboxes.

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `DAC_E2B_BASE_URL` | yes | | E2B API origin, such as `https://api.e2b.app`. |
| `DAC_E2B_API_KEY_FILE` | yes | | File holding the E2B team API key. |
| `DAC_E2B_TEMPLATE_ID` | yes | | The E2B template each sandbox is created from. |
| `DAC_E2B_STATE` | yes | | Path of the adapter's persistent journal file. |
| `DAC_E2B_SANDBOX_TIMEOUT` | no | `1h` | How long a sandbox may live before E2B stops it. The Hobby plan allows up to `1h`, the Pro plan up to `24h`; a longer value fails every provision. |
| `DAC_ENVIRONMENT_ATTACH_KEY_FILE` | no | | File holding the key shared with the runtime adapter. When set, each sandbox starts an exec-server the runtime adapter attaches to, which needs a template that includes it. |

## dac-s3-adapter

The artifact store adapter for an S3-compatible object store. `dac-s3-adapter provision` prepares the two buckets
with the operator's key; plain `dac-s3-adapter` serves with the adapter's key.

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `DAC_S3_ENDPOINT` | yes | | HTTPS origin where the adapter reaches the store. |
| `DAC_S3_PUBLIC_ENDPOINT` | no | `DAC_S3_ENDPOINT` | HTTPS origin where the runtime reaches the store. Upload URLs are signed for this host. |
| `DAC_S3_REGION` | no | `us-east-1` | Signing region. |
| `DAC_S3_ACCESS_KEY_ID` | yes | | Access key ID: the adapter's key to serve, the operator's key to provision. |
| `DAC_S3_SECRET_ACCESS_KEY_FILE` | yes | | File holding the secret for `DAC_S3_ACCESS_KEY_ID`. |
| `DAC_S3_STAGING_BUCKET` | yes | | Bucket that receives uploads before they are committed. |
| `DAC_S3_OBJECTS_BUCKET` | yes | | Bucket that holds committed artifacts, under Object Lock. Must differ from the staging bucket. |
| `DAC_S3_CA_FILE` | no | system roots | PEM bundle of the roots that sign the store's certificate. |
| `DAC_S3_UPLOAD_TTL` | no | `15m` | How long an upload URL stays valid. At most `168h`. |
| `DAC_S3_RETENTION_DAYS` | no | `30` | `provision` only: default retention of committed artifacts, in days. |
| `DAC_S3_STAGING_DAYS` | no | `2` | `provision` only: days before an uncommitted upload expires. It must outlast the 24-hour artifact deadline plus `DAC_S3_UPLOAD_TTL`. |

## Identity configuration file

`DAC_IDENTITY_CONFIG_FILE` names a JSON file listing the OpenID Connect issuers whose access tokens dac-plane
accepts. Its schema is
[`identity-config.schema.json`](https://github.com/dac-plane/dac-plane/blob/main/contracts/jsonschema/config/identity-config.schema.json).

```json
{
  "issuers": [
    {
      "issuer": "https://id.example.com/realms/dac",
      "audience": "dac-plane-surface",
      "algorithms": ["RS256"],
      "max_token_lifetime_seconds": 900
    }
  ],
  "adapter_issuers": [
    {
      "issuer": "https://id.example.com/realms/dac",
      "audience": "dac-plane-adapters",
      "algorithms": ["RS256"],
      "max_token_lifetime_seconds": 900
    }
  ]
}
```

`issuers` authenticate Surface API callers and must list at least one. `adapter_issuers` authenticate adapters; with
none, no adapter can connect. One issuer may appear in both lists, but with a different audience in each, so a token
for one cannot be used on the other. Each entry has:

| Field | Required | Default | Meaning |
|---|---|---|---|
| `issuer` | yes | | The issuer URL: `https`, with no query or fragment. It must match the tokens' `iss` claim. |
| `audience` | yes | | The audience tokens must carry, 1 to 2048 visible ASCII characters. |
| `algorithms` | yes | | Signing algorithms to accept, from `RS256`, `RS384`, `RS512`, `PS256`, `PS384`, `PS512`, `ES256`, `ES384`, `ES512` and `EdDSA`. Shared-secret algorithms are never accepted. |
| `max_token_lifetime_seconds` | yes | | Longest token lifetime accepted, 1 to 86400. |
| `clock_skew_seconds` | no | `30` | Clock difference tolerated when checking token times, 0 to 300. |
| `require_access_token_type` | no | `true` | Require tokens typed `at+jwt` (RFC 9068). Turn it off only for an issuer that cannot type access tokens, and use an audience that issuer grants for nothing else. |
| `development` | no | `false` | Marks the development profile's test issuer. A production deployment refuses a file that sets it. |
