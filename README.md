# Open Source Control Plane for Delegated Agent Work

dac-plane keeps the durable record of work you delegate to AI agents: tasks, attempts, approvals, budgets, artifacts, audit, results and reviews. The agent runtime and the environment it runs in stay replaceable.

It currently runs Codex in E2B sandboxes with S3-compatible artifact storage, and other providers can connect through the [adapter kit](adapterkit/README.md). It is ready for self-hosted pilots. See the [known limits](docs/releases/v0.1.0.md#known-limits) before you rely on it.

## Documentation

* [Deployment guide](deploy/compose/README.md) - configuring, upgrading and rolling back a deployment
* [Surface API](contracts/openapi/surface-v1.json) - the OpenAPI document for the public API
* [Adapter kit](adapterkit/README.md) - connecting a new runtime, environment or artifact store
* [Architecture](docs/architecture/README.md) - how the system is built, and the [decisions](docs/adr) behind it
* [Releases](RELEASES.md) - versioning, compatibility and how to verify a release

## Reporting security vulnerabilities

If you have found a security vulnerability, follow the [instructions on how to report it](SECURITY.md). Do not open a public issue.

## Reporting an issue

If you believe you have found a defect, open [an issue](https://github.com/dac-plane/dac-plane/issues) with a summary, what you expected and the steps to reproduce it.

## Getting started

Each [release](https://github.com/dac-plane/dac-plane/releases/latest) lists its images by digest in `release-manifest.json`, and the images are published as [packages](https://github.com/dac-plane/dac-plane/packages). To deploy them, follow the [deployment guide](deploy/compose/README.md).

To try it from source against simulated providers, with no provider accounts:

```sh
just stack-up
```

## Building from source

To build the project and run its tests, refer to the [development guide](docs/development.md).

## Contributing

Before contributing, read the [contributing guidelines](CONTRIBUTING.md). Work starts from an [issue](https://github.com/dac-plane/dac-plane/issues), and a change to the architecture gets a new [decision record](docs/adr).

## License

[Apache License, Version 2.0](LICENSE). Third-party notices are in [third_party](third_party) and in `/licenses` in each published image.
