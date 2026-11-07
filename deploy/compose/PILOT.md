# Operating a dac-plane pilot

The runbook that was here is now part of the user guide, published from
[`docs/guides/`](../../docs/guides/index.md):

| Former step | Now |
|---|---|
| What runs where, prerequisites, steps 1 to 7 (lay out, generate, credentials, identity and provisioning, deploy, verify, reach from another machine) | [Deploy](../../docs/guides/deploy/index.md) |
| Steps 8 to 12 (inspect, back up and restore, upgrade and roll back, rotate credentials, known problems) and limits | [Operate](../../docs/guides/operate/index.md) |

The step numbers are unchanged, so references such as "PILOT.md step 10" in
[the #96 evidence](../../docs/architecture/pilot-96.md) find the same procedure under the same number.

The files the runbook uses stay here: `compose.pilot.yaml`, `compose.keycloak.yaml`, `generate-pilot-config.sh` and
`configure-pilot.sh`.
