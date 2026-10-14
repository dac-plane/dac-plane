# integrations/occ (WP03) — archived after failed qualification

This directory preserves the proposed public-API mapping to OpenClaw Enterprise/OCC as qualification
evidence. It is not an active integration target. Q1 proved the Cilium network boundary but the dedicated
Codex readiness path ignored the configured custom provider base URL, so ADR-0012 rejected OCC for the
current architecture.

Do not implement this mapping unless a new ADR reopens OCC with upstream evidence that the exact failed
provider-routing path is fixed. The reusable rule remains: integrations use public APIs, never private
candidate tables, and preserve the original actor and product-owned task state.
