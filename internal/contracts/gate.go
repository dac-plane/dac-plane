package contracts

import (
	"fmt"
	"slices"
)

// Negotiate selects the highest protocol version both peers support and the
// capabilities both offer. It fails, with no downgrade, when no version is
// shared or a capability either peer requires is not offered by the other.
func Negotiate(local, peer Lifecycle) (Lifecycle, *Failure) {
	var version uint32
	for _, candidate := range local.Versions {
		if slices.Contains(peer.Versions, candidate) && candidate > version {
			version = candidate
		}
	}
	if version == 0 {
		return Lifecycle{}, failure(FailureProtocolMismatch, "no common protocol version")
	}
	for _, required := range local.Required {
		if !slices.Contains(peer.Offered, required) {
			return Lifecycle{}, failure(FailureUnsupportedCapability, "peer does not offer required capability %s", required)
		}
	}
	for _, required := range peer.Required {
		if !slices.Contains(local.Offered, required) {
			return Lifecycle{}, failure(FailureUnsupportedCapability, "local side does not offer required capability %s", required)
		}
	}
	var selected []Capability
	for _, capability := range local.Offered {
		if slices.Contains(peer.Offered, capability) {
			selected = append(selected, capability)
		}
	}
	return Lifecycle{Phase: PhaseReady, Version: version, Selected: selected}, nil
}

type gateState int

const (
	awaitingHello gateState = iota
	open
	closed
)

// Gate admits a provider's frames on the product side of one session. The
// provider's first frame must be a valid hello; the gate answers with ready or
// shutdown. No request, cancel, reply or event is admitted before ready, and
// any malformed or out-of-order frame shuts the session down with a typed
// failure, so a protocol error stops the session before provider work starts.
// A Gate is not safe for concurrent use.
type Gate struct {
	local    Lifecycle
	state    gateState
	selected Lifecycle
}

// NewGate returns a gate that will negotiate with the given hello.
func NewGate(local Lifecycle) (*Gate, error) {
	if local.Phase != PhaseHello {
		return nil, fmt.Errorf("%w: a gate is configured with a hello", ErrInvalid)
	}
	if err := local.Validate(); err != nil {
		return nil, err
	}
	return &Gate{local: local}, nil
}

// Selected returns the negotiated version and capabilities, once open.
func (g *Gate) Selected() (Lifecycle, bool) {
	return g.selected, g.state == open
}

// Receive admits one frame. It returns a frame to send back, if any, and a
// failure when the frame was refused; a refusal closes the session and the
// returned frame is the shutdown to send.
func (g *Gate) Receive(frame Frame) (*Frame, *Failure) {
	if g.state == closed {
		return nil, failure(FailureTransportUnavailable, "session is closed")
	}
	if err := frame.Validate(); err != nil {
		return g.shutdown(failure(FailureMalformedFrame, "%v", err))
	}
	if frame.Kind == FrameLifecycle && frame.Lifecycle.Phase == PhaseShutdown {
		g.state = closed
		return nil, nil
	}
	switch g.state {
	case awaitingHello:
		if frame.Kind != FrameLifecycle || frame.Lifecycle.Phase != PhaseHello {
			return g.shutdown(failure(FailureProtocolMismatch, "%s frame before hello", frame.Kind))
		}
		ready, fail := Negotiate(g.local, *frame.Lifecycle)
		if fail != nil {
			return g.shutdown(fail)
		}
		g.state, g.selected = open, ready
		return &Frame{Kind: FrameLifecycle, Lifecycle: &ready}, nil
	default:
		if frame.Kind == FrameLifecycle {
			return g.shutdown(failure(FailureProtocolMismatch, "%s after the session opened", frame.Lifecycle.Phase))
		}
		return nil, nil
	}
}

func (g *Gate) shutdown(fail *Failure) (*Frame, *Failure) {
	g.state = closed
	return &Frame{Kind: FrameLifecycle, Lifecycle: &Lifecycle{Phase: PhaseShutdown, Failure: fail}}, fail
}
