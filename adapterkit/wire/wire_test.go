package wire

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const id = "01a167fd-51d9-76ba-b563-496e2bd89646"

func TestEmbeddedSchemasCompile(t *testing.T) {
	if _, err := frameSchema(); err != nil {
		t.Fatalf("embedded schemas: %v", err)
	}
}

func TestDecodeAcceptsEveryFrameKind(t *testing.T) {
	cursor := Cursor{Tenant: id, Attempt: id, Operation: id, Stream: "dispatch", Generation: 1}
	payload := json.RawMessage(`{"text":"hi"}`)
	frames := map[string]Frame{
		"hello":       {Kind: KindLifecycle, Lifecycle: &Lifecycle{Phase: PhaseHello, Versions: []uint32{1}, Offered: []string{"stream.replay"}}},
		"ready":       {Kind: KindLifecycle, Lifecycle: &Lifecycle{Phase: PhaseReady, Version: 1}},
		"shutdown":    Shutdown(&Failure{Code: FailureMalformedFrame, Message: "bad"}),
		"request":     {Kind: KindRequest, Request: &Request{Operation: id, Delivery: id, Tenant: id, Attempt: id, Role: RoleRuntime, Method: "start_turn", Demand: "person", Deadline: time.Now().UTC()}},
		"cancel":      {Kind: KindCancel, Cancel: &Cancel{Operation: id, Delivery: id}},
		"reply":       {Kind: KindReply, Reply: &Reply{Operation: id, Delivery: id, Outcome: OutcomeOK, Durability: DurabilityAccepted}},
		"subscribe":   {Kind: KindSubscribe, Subscribe: &Subscribe{Cursor: cursor}},
		"unsubscribe": {Kind: KindUnsubscribe, Unsubscribe: &Unsubscribe{Operation: id, Stream: "dispatch"}},
		"event": {Kind: KindEvent, Event: &Event{Tenant: id, Attempt: id, Provider: id, Operation: id, Stream: "dispatch", Generation: 1,
			Body: StreamFrame{Kind: StreamRows, Rows: &Rows{Entries: []Entry{{Sequence: 1, UpdatedSequence: 1, Kind: "turn_started", PayloadHash: PayloadDigest(payload), Payload: payload}}}}}},
	}
	for name, frame := range frames {
		t.Run(name, func(t *testing.T) {
			data, err := Encode(frame)
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			if _, err := Decode(data); err != nil {
				t.Fatalf("Decode(%s) error = %v", data, err)
			}
		})
	}
}

func TestDecodeRefusesMalformedFrames(t *testing.T) {
	payload := `{"text":"hi"}`
	for name, data := range map[string]string{
		"empty":              ``,
		"array":              `[]`,
		"null":               `null`,
		"unknown kind":       `{"kind":"notify"}`,
		"unknown field":      `{"kind":"cancel","cancel":{"operation_id":"` + id + `","delivery_id":"` + id + `"},"extra":1}`,
		"two bodies":         `{"kind":"cancel","cancel":{"operation_id":"` + id + `","delivery_id":"` + id + `"},"unsubscribe":{"operation_id":"` + id + `","stream":"s"}}`,
		"trailing document":  `{"kind":"cancel","cancel":{"operation_id":"` + id + `","delivery_id":"` + id + `"}}{}`,
		"non-v7 id":          `{"kind":"cancel","cancel":{"operation_id":"01928f6e-7c3a-4b1e-9f00-0123456789ab","delivery_id":"` + id + `"}}`,
		"required unoffered": `{"kind":"lifecycle","lifecycle":{"phase":"hello","versions":[1],"required":["hooks"]}}`,
		"payload hash":       `{"kind":"event","event":{"tenant_id":"` + id + `","attempt_id":"` + id + `","provider_id":"` + id + `","operation_id":"` + id + `","stream":"s","generation":1,"body":{"kind":"rows","rows":{"entries":[{"sequence":1,"updated_sequence":1,"kind":"k","payload_hash":"` + PayloadDigest([]byte(`{}`)) + `","payload":` + payload + `}]}}}}`,
		"wrong method":       `{"kind":"request","request":{"operation_id":"` + id + `","delivery_id":"` + id + `","tenant_id":"` + id + `","attempt_id":"` + id + `","role":"runtime","method":"provision","demand":"person","deadline":"2026-10-24T12:00:00Z"}}`,
		"oversize arguments": `{"kind":"request","request":{"operation_id":"` + id + `","delivery_id":"` + id + `","tenant_id":"` + id + `","attempt_id":"` + id + `","role":"runtime","method":"start_turn","demand":"person","deadline":"2026-10-24T12:00:00Z","arguments":{"x":"` + strings.Repeat("a", 64<<10) + `"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(data)); !errors.Is(err, ErrMalformed) {
				t.Fatalf("Decode() error = %v, want ErrMalformed", err)
			}
		})
	}
}
