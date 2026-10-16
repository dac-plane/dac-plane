package worker

import (
	"context"
	"io"
	"sync"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// LoopbackPort is the test-only provider port the worker tests script.
// It records every call, validates inbound frames, and replays scripted
// replies, errors and stream frames per operation. It must never leave
// internal/worker: simulator parity is #22's job, and a fake provider in
// production code would silently satisfy dispatches with no effects.
type LoopbackPort struct {
	mu       sync.Mutex
	role     contracts.Role
	sent     []contracts.Request
	cancels  []contracts.Cancel
	subs     []contracts.Cursor
	replies  map[string]loopbackReply
	frames   map[string][]contracts.StreamFrame
	failSend map[string]error
	failStop map[string]error
}

type loopbackReply struct {
	outcome    contracts.Outcome
	durability lifecycle.SendStatus
	failure    *contracts.Failure
	result     []byte
}

// NewLoopbackPort returns a port that answers with ok/accepted unless a
// script says otherwise.
func NewLoopbackPort(role contracts.Role) *LoopbackPort {
	return &LoopbackPort{
		role: role, replies: map[string]loopbackReply{},
		frames:   map[string][]contracts.StreamFrame{},
		failSend: map[string]error{}, failStop: map[string]error{},
	}
}

// ScriptReply programs the reply for one operation. A nil failure with an
// ok outcome still needs an accepted or pending durability; anything else
// is returned verbatim so tests can prove the worker rejects it.
func (p *LoopbackPort) ScriptReply(operation ids.OperationID, outcome contracts.Outcome, durability lifecycle.SendStatus, failure *contracts.Failure, result []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.replies[operation.String()] = loopbackReply{outcome: outcome, durability: durability, failure: failure, result: result}
}

// ScriptSendError programs a transport loss for one operation: Send returns
// the error and the worker must reconcile instead of guessing.
func (p *LoopbackPort) ScriptSendError(operation ids.OperationID, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failSend[operation.String()] = err
}

// ScriptCancelError programs a Cancel failure for one operation.
func (p *LoopbackPort) ScriptCancelError(operation ids.OperationID, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failStop[operation.String()] = err
}

// ScriptFrames programs the stream frames one Subscribe returns for an
// operation, in order.
func (p *LoopbackPort) ScriptFrames(operation ids.OperationID, frames ...contracts.StreamFrame) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frames[operation.String()] = append(p.frames[operation.String()], frames...)
}

// Role reports the provider role this port answers for.
func (p *LoopbackPort) Role() contracts.Role {
	return p.role
}

// Sent returns the requests delivered so far, in order.
func (p *LoopbackPort) Sent() []contracts.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]contracts.Request(nil), p.sent...)
}

// Cancels returns the cancels delivered so far, in order.
func (p *LoopbackPort) Cancels() []contracts.Cancel {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]contracts.Cancel(nil), p.cancels...)
}

// Send records the request and replays the scripted reply, defaulting to
// ok/accepted with the request's own identities stamped in.
func (p *LoopbackPort) Send(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	if err := ctx.Err(); err != nil {
		return contracts.Reply{}, err
	}
	if err := request.Validate(); err != nil {
		return contracts.Reply{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, request)
	if err, ok := p.failSend[request.Operation.String()]; ok {
		return contracts.Reply{}, err
	}
	scripted, ok := p.replies[request.Operation.String()]
	if !ok {
		return contracts.Reply{
			Operation: request.Operation, Delivery: request.Delivery,
			Outcome: contracts.OutcomeOK, Durability: lifecycle.SendAccepted,
		}, nil
	}
	reply := contracts.Reply{
		Operation:  request.Operation,
		Delivery:   request.Delivery,
		Outcome:    scripted.outcome,
		Durability: scripted.durability,
		Failure:    scripted.failure,
		Result:     scripted.result,
	}
	return reply, nil
}

// Cancel records the cancel and replays a scripted failure, if any.
func (p *LoopbackPort) Cancel(ctx context.Context, cancel contracts.Cancel) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cancel.Validate(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancels = append(p.cancels, cancel)
	return p.failStop[cancel.Operation.String()]
}

// Subscribe records the cursor and returns the scripted frames for its
// operation, ending in io.EOF like a closed stream.
func (p *LoopbackPort) Subscribe(ctx context.Context, cursor contracts.Cursor) (contracts.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := cursor.Validate(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subs = append(p.subs, cursor)
	return &loopbackStream{frames: append([]contracts.StreamFrame(nil), p.frames[cursor.Operation.String()]...)}, nil
}

// loopbackStream replays scripted frames then ends.
type loopbackStream struct {
	frames []contracts.StreamFrame
	next   int
}

// Next returns the next scripted frame, or io.EOF when drained.
func (s *loopbackStream) Next(ctx context.Context) (contracts.StreamFrame, error) {
	if err := ctx.Err(); err != nil {
		return contracts.StreamFrame{}, err
	}
	if s.next >= len(s.frames) {
		return contracts.StreamFrame{}, io.EOF
	}
	frame := s.frames[s.next]
	s.next++
	return frame, nil
}

// Close ends the stream.
func (s *loopbackStream) Close() error {
	return nil
}
