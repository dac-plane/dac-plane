# Command line reference

The `dac-plane` image runs one binary, `dac-plane`, with a command as its first argument. Every command reads its
settings from the environment; the [configuration reference](configuration.md) lists them.

```text
dac-plane <command> [arguments]
```

A test checks that every command and provisioning operation the binary accepts has a section here.

## `serve`

Runs the Surface API, the adapter endpoint, the operations listener and the dispatch worker. It is the default when
no command is given, and takes no arguments. It needs the database, the identity configuration and the TLS
certificate and key. It refuses to start against a database whose schema version is outside the range the release
supports.

## `migrate`

```text
dac-plane migrate up | status | down-to VERSION
```

| Argument | Effect |
|---|---|
| `up` | Applies every pending schema migration. |
| `status` | Prints the database's schema version and the range this release supports. |
| `down-to VERSION` | Reverts to `VERSION`. A step that would discard recorded evidence is refused. |

A run holds a PostgreSQL advisory lock, so a second run waits for the first. Upgrade the serving binaries before
migrating; see [Operate](../operate/index.md).

## `health`

```text
dac-plane health live | ready
```

Probes the local operations listener at `DAC_OPS_ADDR` and exits 0 when it answers: `live` asks whether the process
is running, `ready` whether it can serve. Container health checks use it, since the image has no shell. It reads no
secrets.

## `version`

Prints the release version, source revision, supported schema range and adapter protocol version. Takes no arguments.

## `provision`

```text
dac-plane provision <operation> [flags]
```

Trusted provisioning: the operator's way to create tenants and principals, register providers and bind identities.
It writes the database directly, so run it only where the database credentials are. Every operation records what it
changed, is idempotent, and prints one JSON object naming the affected records. Pass `-id` to make a creation
repeatable with a known identifier.

Exit status is 0 on success, 1 when the operation fails (such as a conflict with an existing record) and 2 for
invalid arguments, checked before the database is opened.

### `tenant`

```text
dac-plane provision tenant [-id TENANT]
```

Creates a tenant.

### `principal`

```text
dac-plane provision principal -tenant TENANT -kind human|service [-id ID]
```

Creates a principal of the given kind in an existing tenant. Repeating it with the same `-id` and kind changes
nothing; a different kind is a conflict.

### `bind`

```text
dac-plane provision bind -tenant TENANT -principal ID -issuer URL -subject SUB
```

Binds an identity, the issuer and the token's `sub` claim, to a principal, so tokens for that subject act as that
principal. An identity already bound to another principal of the tenant is a conflict: unbind it first.

### `unbind`

```text
dac-plane provision unbind -tenant TENANT -issuer URL -subject SUB
```

Disables an identity binding. Tokens for that subject are refused from then on.

### `grant-reader`

```text
dac-plane provision grant-reader -tenant TENANT -principal ID
```

Lets a principal read every task in the tenant, not only the tasks it submitted.

### `revoke-reader`

```text
dac-plane provision revoke-reader -tenant TENANT -principal ID
```

Removes the permission `grant-reader` gives.

### `register-provider`

```text
dac-plane provision register-provider -tenant TENANT -role ROLE [-id ID] [-artifact-store ID]
```

Registers a provider in one of the roles `runtime`, `environment`, `enforcement` or `artifact_store`. A provider can
be registered in several roles. A runtime names the artifact store it uploads to with `-artifact-store`, which must
already be registered in that role. Registering a provider again in the same role changes only its artifact store.

### `bind-adapter`

```text
dac-plane provision bind-adapter -tenant TENANT -provider ID -role ROLE -issuer URL -subject SUB
```

Binds an adapter's identity, its client's issuer and subject, to one provider registration, so that adapter can
connect as that provider in that role. Give each adapter registration its own client: an identity already bound to
another registration is a conflict.

### `unbind-adapter`

```text
dac-plane provision unbind-adapter -issuer URL -subject SUB
```

Disables an adapter's binding. The adapter can no longer connect.

### `qualify-harness`

```text
dac-plane provision qualify-harness -tenant TENANT -provider ID -mode live|shadow
```

Seeds a registered runtime's first harness qualification, which dispatch needs before it sends the runtime any work.
In `live` mode the runtime's work counts. In `shadow` mode it may record comparison evidence but cannot change the
record or perform governed actions outside. Repeating it in the same mode changes nothing.
Later changes of mode go through migration passes, not provisioning.
