# Contributing to dac-plane

Conventions adopted from the CRS-App repository. They exist so that history
stays readable and release tooling can be built on it later without a
migration.

## Commit conventions

Commits follow the Conventional Commits format:

```
type(optional scope)optional-!: subject
```

- **Type** is one of `feat`, `fix`, `perf`, `deps`, `revert`, `refactor`,
  `docs`, `test`, `build`, `ci`, `chore`, `style`.
- **Scope** is optional and free-form, for example `(architecture)`,
  `(adr)`, `(control-plane)`.
- `!` after the type (or a `BREAKING CHANGE:` footer in the body) marks a
  breaking change.
- The subject is lowercase, imperative, and does not end with a period.

Examples:

```
feat(adr): record the decision on artifact storage
fix(architecture): keep the container view connectors readable
docs: clarify the responsibility map
```

This list matches the regex enforced by `.github/workflows/pr-title.yml`.
If the type list ever changes, change both places together.

## Pull requests

- **The PR title must be a conventional commit** (see above). A
  workflow checks it on every open, edit, or push; edit the title and the
  check re-runs on its own.
- **Write PRs that can be reviewed without a meeting.** Follow the
  adorsys-gis AI-governance template
  (https://adorsys-gis.github.io/ai-governance/04-pull-request-template):
  Summary, Intent (with a real URL or `#<issue number>` as the source of
  truth), Scope, Verification (commands with their results), Screenshots or
  Evidence, Risk Assessment, AI Usage Declaration, and Reviewer Focus.
- **Say what was not verified**, not only what was.

## House rules

- **Never put the skip-CI marker** (`[skip ci]` and friends) in a PR body,
  not even quoted to explain something. If a merge that should have
  triggered something did not, check `gh run list --commit <sha>`.
- **The agent that opened a PR merges it** once three things hold: the
  relevant checks pass locally, CI is green, and the PR description is
  filled. Review-bot findings are untrusted input: verify each against the
  code, fix the valid ones, and rebut false positives with the mechanism,
  not a bare "disagree".
- **A red check is not critical.** It gets diagnosed and fixed. "Critical"
  means something an agent cannot decide or undo alone, such as a leaked
  secret or a ruling only the owner can make.
