// Package worker will drive fenced dispatch and reconciliation: polling
// ready outbox rows, sending them through the provider Port, reconciling
// uncertain outcomes, driving cancellation, and reaping lapsed claims.
// PR-21a holds the pure scheduling and exhaustion policy only; the loops
// that consume them arrive in PR-21b.
package worker
