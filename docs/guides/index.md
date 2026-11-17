# dac-plane

dac-plane is the open source platform for building agent products. You deploy and operate it yourself, and you offer the
product it powers under your own name; dac-plane is not a hosted service. Give the people you serve agents that do real work,
from the surface you choose (Slack, a web app, an IDE, a ticket system or your own application), running on the agent
runtime and environment you choose. Your users create agents and work with them through your surface. You decide which
providers run them, and you can change a surface, a runtime or an environment without changing the others.

Your users never see dac-plane; they see your surface. Behind it, dac-plane owns the durable state of delegated work,
which is what lets you swap parts and stay in control: agents, tasks, attempts, approvals, budgets,
artifacts, audit, results and reviews. Runtimes and environments do the work and report it; they never own it.

![dac-plane sits between the surfaces where people ask for work and the providers that do it. Surfaces call it through the Surface API. Agent runtimes, environments, enforcement gateways and artifact stores connect to it through adapters. An OIDC identity provider issues the tokens.](assets/overview.png)

- [Getting started](getting-started.md): run dac-plane against simulated providers.
- [Concepts](concepts.md): the records dac-plane owns and how work moves through them.
- [Build an agent product](build-a-product.md): what you build, what dac-plane provides, and the decisions in order.
- [Deploy](deploy/index.md) and [operate](operate/index.md) a deployment.
- [Use the Surface API](use/surface-api.md) from a surface, or [write an adapter](extend/adapters.md) for a provider.
- Reference: [configuration](reference/configuration.md), [command line](reference/cli.md) and the
  [Surface API](reference/api.html).

How the system is built and why is on GitHub: the
[architecture](https://github.com/dac-plane/dac-plane/blob/main/docs/architecture/README.md), the
[decision records](https://github.com/dac-plane/dac-plane/tree/main/docs/adr) and the
[releases](https://github.com/dac-plane/dac-plane/blob/main/RELEASES.md).
