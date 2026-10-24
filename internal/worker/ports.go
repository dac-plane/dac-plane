package worker

import (
	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// Ports resolves the port of a tenant's provider in one role. A provider may
// hold ports in several roles under one identity: a harness provider is both
// a runtime and an environment. Port returns nil when the provider has no
// port in that role now; with the wire transport that means no live
// connection on this replica, so the worker claims no work for it.
type Ports interface {
	Port(tenant ids.TenantID, provider ids.ProviderID, role contracts.Role) contracts.Port
	// ArtifactStore returns the artifact store a runtime exports to.
	ArtifactStore(tenant ids.TenantID, producer ids.ProviderID) (ArtifactStorePort, bool)
}

// WithPorts resolves every provider port through ports instead of the maps
// given to New and the role options.
func WithPorts(ports Ports) func(*Worker) {
	return func(w *Worker) { w.lookup = ports }
}

// port returns the provider's port in role, or nil.
func (w *Worker) port(tenant ids.TenantID, provider ids.ProviderID, role contracts.Role) contracts.Port {
	if w.lookup != nil {
		return w.lookup.Port(tenant, provider, role)
	}
	var byRole map[ids.ProviderID]contracts.Port
	switch role {
	case contracts.RoleEnvironment:
		byRole = w.environments
	case contracts.RoleEnforcement:
		byRole = w.enforcement
	}
	for _, ports := range []map[ids.ProviderID]contracts.Port{byRole, w.ports} {
		if port := ports[provider]; port != nil && port.Role() == role {
			return port
		}
	}
	return nil
}

// artifactStore returns the artifact store a runtime exports to.
func (w *Worker) artifactStore(tenant ids.TenantID, producer ids.ProviderID) (ArtifactStorePort, bool) {
	if w.lookup != nil {
		return w.lookup.ArtifactStore(tenant, producer)
	}
	binding, ok := w.artifacts[producer]
	return binding, ok
}
