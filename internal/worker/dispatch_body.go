package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// errInvalidBody reports a dispatch envelope that parses but names no work.
var errInvalidBody = errors.New("invalid dispatch body")

// DispatchBody is the JSON envelope in an operation-dispatch outbox payload.
// It carries what the dispatch loop needs beyond the outbox row itself: the
// provider operation to drive, the provider call to make, and the budget to
// charge. Producers (#23+) write it; the worker never invents it.
type DispatchBody struct {
	// Operation is the stable provider-operation identity. It must equal the
	// outbox row's record id; a mismatch is a producer error.
	Operation ids.OperationID
	// Role and Method name the provider call.
	Role   contracts.Role
	Method contracts.Method
	// Arguments is the bounded JSON-object call payload, possibly empty.
	Arguments json.RawMessage
	// Deadline bounds the provider call.
	Deadline time.Time
	// Unit and Amount select the task budget and the reservation size.
	Unit   lifecycle.Unit
	Amount uint64
	// CancelReason is empty for dispatches. When set, the row asks for
	// protocol cancellation of Operation instead of a provider call.
	CancelReason string
}

// ParseDispatchBody decodes and checks the envelope. Method and role
// membership is left to contracts.Request.Validate at send time, which
// reports the exact breach.
func ParseDispatchBody(payload []byte) (DispatchBody, error) {
	var raw struct {
		Operation    string           `json:"operation"`
		Role         contracts.Role   `json:"role"`
		Method       contracts.Method `json:"method"`
		Arguments    json.RawMessage  `json:"arguments"`
		Deadline     time.Time        `json:"deadline"`
		Unit         lifecycle.Unit   `json:"unit"`
		Amount       uint64           `json:"amount"`
		CancelReason string           `json:"cancel_reason"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return DispatchBody{}, fmt.Errorf("dispatch body: %w", err)
	}
	operation, err := ids.Parse[ids.Operation](raw.Operation)
	if err != nil {
		return DispatchBody{}, fmt.Errorf("dispatch body operation: %w", err)
	}
	if raw.Role == "" || raw.Method == "" {
		return DispatchBody{}, fmt.Errorf("dispatch body requires role and method: %w", errInvalidBody)
	}
	if raw.Deadline.IsZero() {
		return DispatchBody{}, fmt.Errorf("dispatch body requires a deadline: %w", errInvalidBody)
	}
	return DispatchBody{
		Operation: operation, Role: raw.Role, Method: raw.Method,
		Arguments: raw.Arguments, Deadline: raw.Deadline,
		Unit: raw.Unit, Amount: raw.Amount, CancelReason: raw.CancelReason,
	}, nil
}
