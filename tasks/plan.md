# Documentation site: a user guide published from the repository

Tracker: https://github.com/dac-plane/dac-plane/issues/125
Branch: `docs/docs-site`, from `main` at `46aaf7d`.
Parent: #94. Feeds #97, whose acceptance includes "A new user can follow the documented setup and complete the
representative lifecycle."

## Where the tree stands

Surveyed on 2026-11-07.

| Finding | Evidence | Consequence |
|---|---|---|
| Almost everything under `docs/` is written for contributors and reviewers | `docs/adr/` (19 ADRs), `docs/architecture/` (baseline plus 17 evidence notes such as `pilot-96.md`, `recovery-27.md`), `docs/development.md` | A user finds no guide; the site needs new pages, not only a renderer. |
| Operator instructions are split across two files next to the Compose files | `deploy/compose/README.md` (about 1,300 words) and `deploy/compose/PILOT.md` (about 2,350 words) | They become the deployment and operations guide. Keeping both copies would drift. |
| Provider setup lives in each adapter's README | `adapters/codex/README.md`, `adapters/e2b/README.md`, `adapters/s3/README.md` (about 2,000 words together), `adapterkit/README.md` | The guide gets one operator page per provider; the READMEs stay for code readers and link to it. |
| Configuration is scattered environment variables with no registry | Nine in `internal/config/config.go`; the adapters read theirs in each `cmd/*/main.go`; about 75 distinct `DAC_*` names in non-test Go, some only for simulators or qualification | A hand-written reference, guarded by a test that compares it with the names the code reads. Building a Keycloak-style registry would be a code refactor for a docs issue. |
| The Surface API is described only by its OpenAPI document | `contracts/openapi/surface-v1.json`, already tested against the served routes | The API reference renders that file; it cannot drift from the routes. |
| The CLI is a set of subcommands with no reference | `serve`, `migrate up/down-to`, `health`, `version`, `provision tenant/principal/bind/unbind/grant-reader/register-provider/bind-adapter/unbind-adapter/qualify-harness` in `cmd/dac-plane/` | A CLI reference page. |
| 17 files under `docs/` link to files outside `docs/` | `../../architecture-explore/...`, `../../deploy/compose/README.md`, `../../tasks/plan.md` | A generator that only builds one folder breaks those links if it builds `docs/` whole. |
| Nothing builds or checks docs today | No docs or link recipe in `justfile`; workflows are `ci`, `codeql`, `e2b-qualification`, `pr-title`, `release`, `scorecard`, `security` | The same PR adds a `just docs-check` recipe and a CI job (see the CI memory and `CLAUDE.md`). |

How Keycloak does it (local checkout at `13ef56190b`): task-shaped guides under `docs/guides/`, option tables generated
from the server's configuration code, docs built and tested on PRs by path-filtered workflows with external links in a
separate job, and a separate `keycloak-web` repository that publishes the guides of the latest release to GitHub Pages.
This plan keeps the first three and drops the separate repository, because dac-plane has no site beyond its docs.

## Decisions

Proposed on 2026-11-07, to be agreed before step 2.

1. **Site source is a new `docs/guides/` folder, and only that folder is built.** The ADRs, the architecture baseline
   and the evidence notes stay where they are and are linked from the site to GitHub. Building all of `docs/` would mean
   rewriting links in accepted ADRs and evidence notes; they are read on GitHub today and lose nothing by staying there.
2. **Zensical, pinned exactly.** It renders plain Markdown with the first heading as the page title and rewrites
   relative `.md` links, so every page reads the same on GitHub and on the site. Hugo needs front matter on every page
   (which GitHub shows as a table) and a hand-written link render hook. Material for MkDocs is past its last committed
   support date (5 November 2026). Zensical is at 0.1.0 and has no Python hooks, so step 1 is a spike that proves what
   this plan needs before any content is written. If it fails, Hugo with front matter in `docs/guides/` only is the
   fallback, and this decision comes back to the owner.
3. **One copy of each instruction.** The content of `deploy/compose/README.md` and `PILOT.md` moves into the guide. Both
   files shrink to a file map and a link, so existing links into them, including from `pilot-96.md`, still land
   somewhere useful. Adapter READMEs keep their design and internals and link to the guide for operator settings.
4. **Configuration reference written by hand, checked by a test.** A Go test reads the `DAC_*` string literals in the
   non-test source of each shipped binary (`cmd/dac-plane`, the three adapters) and fails when a name is missing from
   `docs/guides/reference/configuration.md`, or when the page lists a name no binary reads. Names that exist only for
   simulators, the stack test or qualification (`DAC_SIM_*`, `DAC_STACK_*`, `DAC_S3_QUALIFY_*`, `DAC_QUALIFICATION_*`
   and the E2B restart test hooks) sit in an explicit exclusion list in the test, so leaving one out is a reviewed choice.
5. **API reference rendered from `surface-v1.json` by Redoc 2.5.4**, self-hosted, on its own page outside the theme.
   The spec's `$ref`s point at `contracts/jsonschema/`, so the build copies that folder into the site beside it. A
   Content-Security-Policy on the page keeps every request on the site's own origin. See the spike result for why not
   Redoc 3 or Scalar.
6. **Checks.** `just docs-check` builds the site with warnings as errors, checks every internal link and anchor in the
   built HTML with lychee in offline mode, and runs the configuration test. It joins `ci-fast` and gets a CI job.
   External links are checked by a scheduled workflow, not on PRs, so an outside site going down never blocks a merge.
7. **Publishing.** A workflow builds the site and deploys it to GitHub Pages with `actions/upload-pages-artifact` and
   `actions/deploy-pages`, pinned by SHA like the other workflows, at `dac-plane.github.io/dac-plane`. Which ref it
   publishes is the owner's call (see "Owner decisions").

## Steps

1. **Spike the generator.** In `.work/`, build a three-page Zensical site with: a page whose title comes from its first
   heading; relative links between pages and to an anchor; an image reached through a relative path into
   `docs/architecture/`; one link leaving `docs/guides/`; and the API reference page. Pin Zensical and the renderer.
   Verify: titles and navigation come out right with no front matter; relative links resolve; the build fails on a
   broken internal link (natively or through lychee); the out-of-tree image is either served or the build says why; the
   API page renders `surface-v1.json` with no network. Record the result in this plan before step 2.

   **Result, 2026-11-07: passed, Zensical stays.** Zensical 0.1.0, lychee 0.24.2 and Redoc 2.5.4, in `.work/spike/`,
   checked by building with `--strict`, running lychee and loading the site in headless Chromium.
   - Titles come from the first heading with no front matter ("Concepts - dac-plane"); navigation follows `nav:`.
   - Relative `.md` links and anchors are rewritten; `--strict` fails on a link to a missing page, to a page outside
     `docs/guides/`, and on a missing anchor, with file and line.
   - Images are not checked: a missing image and an image reached through `../architecture/` both build silently, the
     second as a broken link. So lychee is required, not optional: `lychee --offline --include-fragments --index-files
     index.html --root-dir site 'site/**/*.html'` reports the missing image and nothing else. Without
     `--index-files index.html` it reports false errors on every `page/#anchor` link.
   - A file symlink into `docs/guides/` is copied into the site as a real file, so the overview diagram stays in
     `docs/architecture/` and `docs/guides/assets/overview.png` links to it. A symlinked folder is not copied.
   - The theme loads Inter and JetBrains Mono from Google Fonts unless `theme.font: false`; the site sets it, so a
     page makes no request outside its origin.
   - Search works offline from the built `search.json`.
   - API reference: Redoc 3.0.1's standalone bundle is an ES module that loads a logo from `cdn.redoc.ly` and sends
     telemetry unless `disable-telemetry` is set. Redoc 2.5.4 (1.1 MB, a classic script) rendered all 20 operations
     once `contracts/jsonschema/` was copied beside the spec; before that it failed on the external `$ref`s, which is
     a property of our multi-file spec, not of Redoc. Its footer logo still requests `cdn.redoc.ly`, which the page's
     Content-Security-Policy refuses. Scalar's package is 56 MB unpacked and was not tried.
   - Consequences for step 2: the build recipe copies `contracts/jsonschema/` into `site/jsonschema/` after Zensical
     runs; lychee and the Redoc bundle are pinned with checksums (lychee from its release `.sha256`, Redoc by its npm
     `sha512` integrity); CI needs Python for Zensical (the wheel is `cp311-abi3`).
2. **Add the skeleton and the checks.** `mkdocs.yml` (the file name Zensical reads), `docs/guides/index.md`, the
   navigation below as stubs, a pinned install under `.work/tools` from a hashed requirements file, `just docs-build`,
   `just docs-serve` and `just docs-check`, and a `docs` job in `ci.yml`.
   Verify: `just docs-check` passes; a deliberately broken link fails it; `actionlint` passes.

   **Done, 2026-11-07.** `docs-check` passes locally (184 internal links, 0 errors); a link to a missing page fails
   the strict build and a missing image fails lychee. Two findings: the generated `404.html` links absolutely under
   `/dac-plane/` (from `site_url`), so lychee remaps that prefix to the build directory; and setting `repo_url` makes
   the header's repository widget call `api.github.com` from the reader's browser. The site is served by GitHub
   Pages, so that request stays with the host; it is the one request a guide page makes outside its origin. Zensical
   writes `.cache/` next to `mkdocs.yml`, now ignored.
3. **Write the configuration and CLI references and the drift test.** Every name, the binary that reads it, whether it
   is required, its default and an example, from the source. The CLI page from each command's flags.
   Verify: the test passes; removing one row fails it; adding a `DAC_` read in a binary without a row fails it.

   **Done, 2026-11-07.** Every variable a released binary reads is in `internal/config` or one adapter module, so the
   test scans those packages (skipping test support, the stack test, qualification runners and examples) and needs
   one exception: `DAC_CODEX_PROVIDER_API_KEY`, the name the Codex adapter gives the key in its child process.
   A second test checks the CLI page against the usage text of `dac-plane` and `provision`. Both were shown to fail
   on a removed row, an undocumented read and a misnamed section. `docs-check` runs them too.
4. **Write the guide.** In order, each page tested as it is written:
   - `index.md`: what dac-plane is, the overview diagram, where to go next.
   - `getting-started.md`: the simulator stack with `just stack-up`, then submit a task, follow its events, answer an
     attention request and review the result with `curl`. Verify: run on a clean clone with only the listed prerequisites.
   - `concepts.md`: the records dac-plane owns, the attempt lifecycle, provider roles, identity. From the architecture
     README and the ADRs, linking to them.
   - `deploy/`: prerequisites, install, identity provider, provisioning, providers (Codex, E2B, S3 store). From
     `deploy/compose/README.md`, `PILOT.md` and the adapter READMEs.
   - `operate/`: health, logs and inspection, backup and restore, upgrade and rollback, credential rotation, known
     limits. From `PILOT.md` and `docs/releases/v0.1.0.md`.
   - `use/surface-api.md`: authentication and each lifecycle call, with the reference page beside it.
   - `extend/adapters.md`: the adapter kit, the four roles and the conformance suite, from `adapterkit/README.md`.
   - Links to architecture, ADRs, releases, contributing and security on GitHub.
   Verify: `just docs-check`; every command in the getting-started and deploy pages was run against the simulator stack
   or cited from the #96 evidence where it needs the real providers.
5. **Shrink the source files.** `deploy/compose/README.md` and `PILOT.md` become a file map and a link; adapter READMEs
   link to the provider pages for settings. The root README's Documentation list points at the site.
   Verify: no instruction exists in two places (`grep` for each moved heading); `just docs-check`; links from
   `pilot-96.md` into `PILOT.md` still resolve.
6. **Publish.** The Pages workflow and the scheduled external-link workflow. `CLAUDE.md` and `CONTRIBUTING.md` say a
   user-facing change updates `docs/guides/` in the same PR.
   Verify: after the owner enables Pages, the workflow deploys; the site opens, search works, the diagram and API page
   render; a PR that breaks a link fails `docs-check`.

## Owner decisions

- **Which ref the site shows.** Recommended: `main`, with the home page naming the latest release and each page that
  describes something not yet released saying so. Publishing only release tags (Keycloak's model) would leave the site
  empty until the next release, because these pages are written after v0.1.0. Switch to per-release versions when a
  second release is supported.
- Agree decisions 1 to 7, in particular moving the `PILOT.md` content into the guide.

## Owner-only actions

- Enable GitHub Pages for the repository with "GitHub Actions" as the source, and allow the `github-pages` environment
  to deploy from the chosen ref.
- A custom domain, if one is wanted later.

## Out of scope

- Versioned docs per release, translations, a blog, a separate website repository.
- Rewriting ADRs or evidence notes for the site.
- A configuration registry in Go; reconsider if the drift test proves too coarse.
- New product behavior. Where the guide finds a gap (for example tenant policy set with SQL), it documents the limit
  and links the issue (#115).

## Limits to state in the PR

- Zensical is pre-1.0; the pin and the spike result are the evidence.
- Getting started runs on simulators. The real-provider pages rest on the #96 pilot evidence, not on a fresh run.

## Tracking issue

Title: `docs: publish a user guide for deploying, operating and integrating dac-plane`

Body: Goal (a new user can deploy, operate and integrate dac-plane from a published guide without reading the source);
Parent #94, before #97; Scope (steps 2 to 6 above, decisions 1 to 7); Acceptance (`just docs-check` in CI with internal
links and the configuration drift test; getting started verified on a clean clone; deployment and operations content
moved, not copied; the site published to GitHub Pages); Out of scope as above.
