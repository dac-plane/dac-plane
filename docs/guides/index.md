# dac-plane

dac-plane keeps the durable record of work you delegate to AI agents: tasks, attempts, approvals, budgets, artifacts,
audit, results and reviews. The agent runtime and the environment it runs in stay replaceable.

![dac-plane sits between the surfaces where people ask for work and the providers that do it. Surfaces call it through the Surface API. Agent runtimes, environments, enforcement gateways and artifact stores connect to it through adapters. An OIDC identity provider issues the tokens.](assets/overview.png)

This guide is being written in [#125](https://github.com/dac-plane/dac-plane/issues/125).

- [Getting started](getting-started.md): run dac-plane against simulated providers.
- [Concepts](concepts.md): the records dac-plane owns and how work moves through them.
- [Deploy](deploy/index.md) and [operate](operate/index.md) a deployment.
- [Use the Surface API](use/surface-api.md) from a surface, or [write an adapter](extend/adapters.md) for a provider.
- Reference: [configuration](reference/configuration.md), [command line](reference/cli.md) and the
  [Surface API](reference/api.html).

How the system is built and why is on GitHub: the
[architecture](https://github.com/dac-plane/dac-plane/blob/main/docs/architecture/README.md), the
[decision records](https://github.com/dac-plane/dac-plane/tree/main/docs/adr) and the
[releases](https://github.com/dac-plane/dac-plane/blob/main/RELEASES.md).
