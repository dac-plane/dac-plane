# Build an agent product on dac-plane

This page is for a company that wants to offer agents to its own users: through Slack, a web app, an IDE or an
application of its own. You deploy and operate dac-plane, and you offer the product under your own name. Your users
see your surface and never dac-plane. This page names the parts of such a product, says which ones you build and which
dac-plane provides, and lists the decisions in the order to make them.

## What you build and what dac-plane provides

| Part | Who provides it |
|---|---|
| The surface your users work in: a chat bot, a web app, an IDE extension or your own application | You. It calls the [Surface API](use/surface-api.md). |
| Sign-in for your users | You. dac-plane does not manage users; it accepts tokens from OpenID Connect issuers you configure. |
| Billing and pricing | You. dac-plane keeps a budget for each task, with what has been reserved and spent. |
| Agents: a name, a role, standing instructions and default providers, revised and retired over time | dac-plane. See [Delegate work to agents](use/agents.md). |
| Tasks, attempts, approvals, results and reviews, with the events that follow them | dac-plane. See [Concepts](concepts.md). |
| Access to your users' external accounts, with the credential kept out of the surface and the runtime | dac-plane and a connector adapter. See [Connect external applications](use/connectors.md). |
| The agent runtime, the environment its work runs in, and the store for its output | A provider you choose, connected through an adapter. dac-plane ships adapters for Codex, E2B and S3-compatible stores; any other provider needs [its own adapter](extend/adapters.md). |

dac-plane owns the record of the work (tasks, approvals, budgets, results, audit), and the providers report what they
observed. That is what lets you change a surface or a provider later without changing the others.

## What your users see

Your users see only your surface. Which parts of the record it shows is your decision. Everything it can show is
available through the Surface API: the agents, each task and its status, the questions an agent is waiting on, the
results to review and the events as work proceeds. A surface that is a chat bot might show a question as a message with
buttons, and a result as a file to download and a review prompt. Nothing in dac-plane requires a screen.

## Decisions, in order

1. **Decide what a tenant is.** Agents, tasks, provider registrations and principals belong to one tenant, and a token
   works only in a tenant where its subject is bound to a principal. One tenant per customer keeps each customer's
   agents, providers and results apart. One tenant for everyone shares agent names and providers among all your users.
   [`provision tenant`](reference/cli.md#provision) creates one.
2. **Decide how your users sign in.** Configure the issuers you trust in the
   [identity configuration file](reference/configuration.md#identity-configuration-file) and bind each user, and each
   service your surface runs as, to a principal with [`provision`](reference/cli.md#provision). A principal reads and
   acts on the tasks it submitted. If your surface acts as one service principal for all its users, that service owns
   every task. Acting for the person who asked is [planned (#152)](https://github.com/dac-plane/dac-plane/issues/152).
3. **Choose the runtime and the environment for your agents.** Register each provider for the tenant and bind its
   adapter. Each agent names a default runtime and environment, and a task can name different ones. You can use more
   than one runtime or environment in a tenant.
4. **Decide who creates agents.** Your surface calls the Surface API to create agents in a tenant. Every principal of
   the tenant can read its agents, two surfaces on one tenant share the same agents, and only the principal that
   created an agent can change it. Users who should not see each other's agents belong in separate tenants.
5. **Deploy and verify.** [Deploy dac-plane](deploy/index.md) with your providers. [Getting started](getting-started.md)
   runs the same lifecycle on your machine against simulated providers, which is a good way to build the surface
   before real providers are connected.
6. **Operate it.** [Operate a deployment](operate/index.md) covers inspection, backup, upgrade and credential rotation.

## What is not built yet

These are open issues. The product you build today cannot rely on them.

| Planned | Issue |
|---|---|
| A surface acting for the person who asked, so a chat bot's tasks are owned by the user | [#152](https://github.com/dac-plane/dac-plane/issues/152) |
| An agent profile with an avatar and a description | [#148](https://github.com/dac-plane/dac-plane/issues/148) |
| Reusable skills for an agent | [#149](https://github.com/dac-plane/dac-plane/issues/149) |
| Memory that lasts across an agent's tasks | [#150](https://github.com/dac-plane/dac-plane/issues/150) |
| Routines that start work on a schedule or an event | [#151](https://github.com/dac-plane/dac-plane/issues/151) |
| A tenant-scoped list of the providers a surface may name | [#156](https://github.com/dac-plane/dac-plane/issues/156) |
| A list of the tasks a principal can read | [#155](https://github.com/dac-plane/dac-plane/issues/155) |
| A reference application that shows a surface working against a deployed dac-plane | [#97](https://github.com/dac-plane/dac-plane/issues/97) |

dac-plane ships no first-party surface. You build yours against the [Surface API](use/surface-api.md).
