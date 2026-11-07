# Open Source Agent Control Plane

dac-plane keeps the durable record of work you delegate to AI agents: tasks, attempts, approvals, budgets, artifacts, audit, results and reviews. The agent runtime and the environment it runs in stay replaceable.

![dac-plane sits between the surfaces where people ask for work and the providers that do it. Surfaces such as web, chat, Slack, IDEs, tickets, schedules, the API and apps call it through the Surface API. Agent runtimes, environments, enforcement gateways and artifact stores connect to it through adapters. An OIDC identity provider issues the tokens.](docs/guides/assets/overview.png)

It currently runs Codex in E2B sandboxes with S3-compatible artifact storage, and other providers can connect through the [adapter kit](adapterkit/README.md). It is ready for self-hosted pilots. See the [known limits](docs/releases/v0.1.0.md#known-limits) before you rely on it.

## Documentation

The [user guide](https://dac-plane.github.io/dac-plane/) is published from [`docs/guides`](docs/guides/index.md):

* [Getting started](docs/guides/getting-started.md) - run it locally and take one task from submission to review
* [Deploy](docs/guides/deploy/index.md) and [operate](docs/guides/operate/index.md) - a deployment with real providers: configuring, backing up, upgrading and rotating credentials
* [Use the Surface API](docs/guides/use/surface-api.md) - the public API, with the [OpenAPI document](contracts/openapi/surface-v1.json)
* [Write an adapter](docs/guides/extend/adapters.md) - connecting a new runtime, environment or artifact store
* [Configuration](docs/guides/reference/configuration.md) and [command line](docs/guides/reference/cli.md) references

Beyond the guide:

* [Architecture](docs/architecture/README.md) - how the system is built, and the [decisions](docs/adr) behind it
* [Releases](RELEASES.md) - versioning, compatibility and how to verify a release

## Reporting security vulnerabilities

If you have found a security vulnerability, follow the [instructions on how to report it](SECURITY.md). Do not open a public issue.

## Reporting an issue

If you believe you have found a defect, open [an issue](https://github.com/dac-plane/dac-plane/issues) with a summary, what you expected and the steps to reproduce it.

## Getting started

Each [release](https://github.com/dac-plane/dac-plane/releases/latest) lists its images by digest in `release-manifest.json`, and the images are published as [packages](https://github.com/dac-plane/dac-plane/packages). To deploy them, follow [Deploy](docs/guides/deploy/index.md).

To try it from source against simulated providers, with no provider accounts, follow
[Getting started](docs/guides/getting-started.md).

## Building from source

To build the project and run its tests, refer to the [development guide](docs/development.md).

## Contributing

Before contributing, read the [contributing guidelines](CONTRIBUTING.md). Work starts from an [issue](https://github.com/dac-plane/dac-plane/issues), and a change to the architecture gets a new [decision record](docs/adr).

## License

[Apache License, Version 2.0](LICENSE). Third-party notices are in [third_party](third_party) and in `/licenses` in each published image.
