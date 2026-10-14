# ADR-0009: Adoption vs fork

- Status: Proposed
- Spec: §26, §43

Prefer composition and declared Driver/plugin extension points. A fork needs a tracked patch inventory,
upgrade owner, compatibility fixtures and sunset criteria. Do not import private OCC queue/IAM packages as
stable SDKs. Pin upstream revisions (spec research pinned OCE at
`07590c64e27ac61c9ad60d9ab61424b70281011a`; re-pin at qualification time).
