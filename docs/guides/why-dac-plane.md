# Why dac-plane

dac-plane is for a company that wants to offer agents to its own users, under its own name: in Slack, a web app, an IDE,
a ticket system or an application of its own. You build the surface and choose the providers. dac-plane runs behind
them and owns the work, from submission to review, so your users see your product and never dac-plane. You do not have to build the machinery that runs and governs the work: the task lifecycle, approvals, budgets, results, review and audit.

For example, a support-software company could ship an in-app agent that takes a customer's request, asks for approval
before it acts, and returns a result the customer reviews, with each customer's agents, budgets and results kept apart.

## What makes it different

* **Headless.** dac-plane ships no user interface. You build the surface against the
  [Surface API](use/surface-api.md).
* **Multi-tenant for your customers.** Agents, tasks, providers and principals belong to a tenant, so one deployment can
  keep many customers apart.
* **It owns the work.** Tasks, attempts, approvals, budgets, artifacts, audit, results and reviews live in
  dac-plane. Runtimes and environments report what they did and never own it, so you can change one without losing the
  history. See [Concepts](concepts.md).
* **Open and self-hosted.** Apache-2.0, deployed and operated by you, with no hosted service and no separate license for
  embedding it in your product.

## What it is not

* **Not an assistant or an agent app.** There is no chat interface to give your users. If you want a finished assistant
  for yourself or your team, use one of those instead.
* **Not a persistent assistant.** dac-plane does not give an agent a conversation or a computer of its own. Work runs as
  tasks, and an environment is stopped when its attempt ends. Memory across tasks and routines are planned
  ([#150](https://github.com/dac-plane/dac-plane/issues/150),
  [#151](https://github.com/dac-plane/dac-plane/issues/151)).
* **Not an agent framework.** It does not define how an agent reasons. A runtime does that, and dac-plane runs and
  governs it.
* **Not a hosted service or a model provider.** You deploy it and bring your own runtimes, environments, stores and
  identity provider.
* **Not a replacement for a cloud vendor's agent platform.** Those host agent code on one cloud. dac-plane sits above
  providers, and such a platform could be one of them.

## How it relates to other tools

| If you are looking at | dac-plane's relationship |
|---|---|
| Finished assistants and coding-agent products, with their own UI and API | These are products you use. dac-plane is what you would build a product like them on. |
| Agent runtimes and SDKs, such as Codex | A runtime connects to dac-plane through an adapter. dac-plane ships one for Codex. |
| Sandbox and environment providers, such as E2B | An environment connects through an adapter. dac-plane ships one for E2B. |
| Applications your agents act in, such as GitHub | An application connects through a connector adapter, which keeps your users' credentials out of the surface and the runtime. dac-plane ships one for GitHub, and more can be added the same way. |
| Cloud vendors' agent hosting platforms | They host and operate agent code. They can serve as a provider behind an adapter, but dac-plane does not require any one cloud. |
| Agent protocols, such as MCP, A2A and ACP | These describe how an agent reaches tools (MCP), other agents (A2A) or an editor (ACP). dac-plane is the system that owns the lifecycle of delegated work. |

This list describes categories, not endorsements, and the field changes quickly. Check each tool's current documentation
before you decide.

## Next

* [Build an agent product](build-a-product.md): what you build, what dac-plane provides, and the decisions in order.
* [Getting started](getting-started.md): run it locally and take one task from submission to review.
