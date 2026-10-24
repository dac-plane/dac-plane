package adapterwire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/AssahBismarkabah/dac-plane/internal/worker"
)

// registrationKey names one provider registration: a provider in one role.
type registrationKey struct {
	tenant   ids.TenantID
	provider ids.ProviderID
	role     contracts.Role
}

// Registry tracks this replica's live adapter connections and resolves the
// worker's ports from them. It implements worker.Ports: a registration with
// no live connection here has no port, so this replica claims no work for it.
type Registry struct {
	options Options
	resolve Resolver
	ctx     context.Context
	cancel  context.CancelFunc

	mu    sync.Mutex
	conns map[registrationKey][]*connection

	droppedReplies atomic.Int64
	droppedEvents  atomic.Int64
}

var _ worker.Ports = (*Registry)(nil)

// NewRegistry returns an empty registry. resolve re-checks each connection's
// binding on every ping tick.
func NewRegistry(resolve Resolver, options Options) (*Registry, error) {
	if resolve == nil {
		return nil, errors.New("adapterwire: resolver is required")
	}
	options = options.withDefaults()
	if err := options.Hello.Validate(); err != nil || options.Hello.Phase != contracts.PhaseHello {
		return nil, fmt.Errorf("adapterwire: hello: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Registry{options: options, resolve: resolve, ctx: ctx, cancel: cancel, conns: map[registrationKey][]*connection{}}, nil
}

// Close shuts every connection down; adapters reconnect to another replica.
func (r *Registry) Close() {
	r.mu.Lock()
	var all []*connection
	for _, conns := range r.conns {
		all = append(all, conns...)
	}
	r.mu.Unlock()
	for _, c := range all {
		c.shutdown(websocket.StatusGoingAway, nil, "control plane shutting down")
	}
	r.cancel()
}

// Port returns the port of a registration with at least one live
// connection, or nil.
func (r *Registry) Port(tenant ids.TenantID, provider ids.ProviderID, role contracts.Role) contracts.Port {
	key := registrationKey{tenant, provider, role}
	if r.Connections(tenant, provider, role) == 0 {
		return nil
	}
	return &providerPort{registry: r, key: key}
}

// ArtifactStore returns the artifact store a connected runtime's
// registration names, when that store is connected too.
func (r *Registry) ArtifactStore(tenant ids.TenantID, producer ids.ProviderID) (worker.ArtifactStorePort, bool) {
	r.mu.Lock()
	var artifactStore *ids.ProviderID
	for _, c := range r.conns[registrationKey{tenant, producer, contracts.RoleRuntime}] {
		if c.registration.ArtifactStore != nil {
			artifactStore = c.registration.ArtifactStore
			break
		}
	}
	r.mu.Unlock()
	if artifactStore == nil {
		return worker.ArtifactStorePort{}, false
	}
	port := r.Port(tenant, *artifactStore, contracts.RoleArtifactStore)
	if port == nil {
		return worker.ArtifactStorePort{}, false
	}
	return worker.ArtifactStorePort{Provider: *artifactStore, Port: port}, true
}

// Connections counts the live connections of a registration.
func (r *Registry) Connections(tenant ids.TenantID, provider ids.ProviderID, role contracts.Role) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns[registrationKey{tenant, provider, role}])
}

// Dropped counts replies no request awaited and events no stream accepted.
func (r *Registry) Dropped() (replies, events int64) {
	return r.droppedReplies.Load(), r.droppedEvents.Load()
}

func (r *Registry) add(c *connection) {
	key := registrationKey{c.registration.Tenant, c.registration.Provider, c.registration.Role}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns[key] = append(r.conns[key], c)
}

func (r *Registry) remove(c *connection) {
	key := registrationKey{c.registration.Tenant, c.registration.Provider, c.registration.Role}
	r.mu.Lock()
	defer r.mu.Unlock()
	conns := r.conns[key]
	for i, candidate := range conns {
		if candidate == c {
			conns = append(conns[:i], conns[i+1:]...)
			break
		}
	}
	if len(conns) == 0 {
		delete(r.conns, key)
	} else {
		r.conns[key] = conns
	}
}

// pick returns the open connection of key with the fewest requests in
// flight. allowDraining admits a draining connection, for a cancel.
func (r *Registry) pick(key registrationKey, allowDraining bool) *connection {
	r.mu.Lock()
	conns := append([]*connection(nil), r.conns[key]...)
	r.mu.Unlock()
	var best *connection
	bestLoad := 0
	for _, c := range conns {
		open, inFlight := c.load()
		if !open && !(allowDraining && c.live()) {
			continue
		}
		if best == nil || inFlight < bestLoad {
			best, bestLoad = c, inFlight
		}
	}
	return best
}

// providerPort is a registration's port over its live connections.
type providerPort struct {
	registry *Registry
	key      registrationKey
}

func (p *providerPort) Role() contracts.Role { return p.key.role }

// Send delivers the request on the least-loaded open connection and waits
// for its reply there. With no open connection nothing is written and the
// error is contracts.ErrNotDelivered.
func (p *providerPort) Send(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	if request.Tenant != p.key.tenant || request.Role != p.key.role {
		return contracts.Reply{}, fmt.Errorf("%w: request is for another registration", contracts.ErrNotDelivered)
	}
	c := p.registry.pick(p.key, false)
	if c == nil {
		return contracts.Reply{}, contracts.ErrNotDelivered
	}
	return c.send(ctx, request)
}

// Cancel writes the cancel on any live connection of the registration.
func (p *providerPort) Cancel(ctx context.Context, cancel contracts.Cancel) error {
	c := p.registry.pick(p.key, true)
	if c == nil {
		return contracts.ErrNotDelivered
	}
	return c.write(ctx, contracts.Frame{Kind: contracts.FrameCancel, Cancel: &cancel})
}

// Subscribe opens the cursor's stream on the least-loaded open connection.
func (p *providerPort) Subscribe(ctx context.Context, cursor contracts.Cursor) (contracts.Stream, error) {
	if cursor.Tenant != p.key.tenant {
		return nil, fmt.Errorf("%w: cursor is for another tenant", contracts.ErrNotDelivered)
	}
	c := p.registry.pick(p.key, false)
	if c == nil {
		return nil, contracts.ErrNotDelivered
	}
	return c.subscribe(ctx, cursor)
}

// wireStream is one subscription's frames, already checked against the
// connection's peer and the cursor's binding.
type wireStream struct {
	connection *connection
	cursor     contracts.Cursor
	key        streamKey
	frames     chan contracts.StreamFrame
	ended      chan struct{}
	endOnce    sync.Once
	cause      error
}

// Next returns the next frame, or an error once the stream ended: the
// connection closed, the subscription was replaced, or Close was called.
func (s *wireStream) Next(ctx context.Context) (contracts.StreamFrame, error) {
	select {
	case frame := <-s.frames:
		return frame, nil
	default:
	}
	select {
	case frame := <-s.frames:
		return frame, nil
	case <-s.ended:
		return contracts.StreamFrame{}, s.cause
	case <-ctx.Done():
		return contracts.StreamFrame{}, ctx.Err()
	}
}

// Close unsubscribes; a frame already in flight is dropped on arrival.
func (s *wireStream) Close() error {
	s.end(io.EOF)
	s.connection.forgetStream(s)
	if !s.connection.live() {
		return nil
	}
	ctx, cancel := context.WithTimeout(s.connection.ctx, time.Second)
	defer cancel()
	unsubscribe := contracts.Unsubscribe{Operation: s.cursor.Operation, Stream: s.cursor.Stream}
	_ = s.connection.write(ctx, contracts.Frame{Kind: contracts.FrameUnsubscribe, Unsubscribe: &unsubscribe})
	return nil
}

func (s *wireStream) end(cause error) {
	s.endOnce.Do(func() {
		s.cause = cause
		close(s.ended)
	})
}

func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}
