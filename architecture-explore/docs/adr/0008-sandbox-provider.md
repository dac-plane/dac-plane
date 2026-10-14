# ADR-0008: Sandbox / compute provider

- Status: Open
- Spec: §17, §38, §40, §42 (Q6)

OCC Kubernetes Compute is no longer an active option after ADR-0012. Select one environment backend only
after the control-plane foundation decision: local/container for trusted work or one managed sandbox
(E2B is a named candidate; OpenSandbox/Daytona alternatives) for stronger isolation. A qualified
control-plane product does not automatically qualify its environment backend.

Do not start with raw Firecracker or install several candidates together. Selection needs Q6 evidence
for processes, files, transport, pause/resume, artifacts, egress, cleanup and cost. Daytona self-host
licensing is not established by the spec's review.
