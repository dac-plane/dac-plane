package simulator

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/AssahBismarkabah/dac-plane/internal/contracts"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
)

// Artifact effect kinds: the external work an artifact store applies once per
// operation.
const (
	UploadAuthorized  EffectKind = "upload_authorized"
	ManifestCommitted EffectKind = "manifest_committed"
)

// artifactEffect maps each effectful artifact method to the effect it applies
// when its scenario applies one.
var artifactEffect = map[contracts.Method]EffectKind{
	contracts.MethodAuthorizeUpload: UploadAuthorized,
	contracts.MethodCommitManifest:  ManifestCommitted,
}

func validArtifactEffect(kind EffectKind) bool {
	switch kind {
	case UploadAuthorized, ManifestCommitted:
		return true
	}
	return false
}

// ArtifactScenario is the script an ArtifactStore follows: one step per
// operation.
type ArtifactScenario struct {
	Operations map[ids.OperationID]ArtifactOp
}

// ArtifactOp is one operation's script. Reply answers Send; a stat or commit
// reply carries the store's observation as StoredReply. Effect is applied
// once, on the first delivery, and must be the effect of the delivered
// method. SendError replaces the reply after the effect, the way a transport
// loses an acknowledgement.
type ArtifactOp struct {
	Reply     Reply
	Effect    EffectKind
	SendError error
}

// ArtifactStore is a deterministic artifact store provider behind
// contracts.Port. It reports what it stores; what a manifest commit means is
// decided by the caller with lifecycle.AdmitArtifact, never here.
type ArtifactStore struct {
	config     Config
	scenario   ArtifactScenario
	ledger     *Ledger
	deliveries *deliveries
}

// NewArtifactStore validates the config and the scenario and returns the
// simulator.
func NewArtifactStore(config Config, scenario ArtifactScenario) (*ArtifactStore, error) {
	config = config.withDefaults()
	if err := config.validate(); err != nil {
		return nil, err
	}
	for _, step := range scenario.Operations {
		if step.Effect != "" && !validArtifactEffect(step.Effect) {
			return nil, fmt.Errorf("%w: %s is not an artifact effect", ErrInvalidScenario, step.Effect)
		}
	}
	return &ArtifactStore{
		config:     config,
		scenario:   scenario,
		ledger:     newLedger(),
		deliveries: newDeliveries(),
	}, nil
}

// StoredReply answers with the store's observation of the uploaded bytes. A
// lifecycle.StoredObject always marshals, so the reply cannot fail.
func StoredReply(object lifecycle.StoredObject) Reply {
	result, _ := json.Marshal(object)
	return Accepted(result)
}

// Role returns contracts.RoleArtifactStore.
func (a *ArtifactStore) Role() contracts.Role {
	return contracts.RoleArtifactStore
}

// Hello returns the lifecycle hello the simulator advertises.
func (a *ArtifactStore) Hello() contracts.Lifecycle {
	return a.config.Hello
}

// Control returns the pause control pacing this simulator's boundaries.
func (a *ArtifactStore) Control() *Control {
	return a.config.Control
}

// Ledger returns the simulator's record of requests, cancels, subscriptions
// and applied effects.
func (a *ArtifactStore) Ledger() *Ledger {
	return a.ledger
}

// Send answers one delivery. The first delivery of an effectful operation
// applies the scripted effect; a redelivery of a finished operation answers
// from the record, so a commit never stores twice.
func (a *ArtifactStore) Send(ctx context.Context, request contracts.Request) (contracts.Reply, error) {
	if err := ctx.Err(); err != nil {
		return contracts.Reply{}, err
	}
	if err := checkRequest(a.config, contracts.RoleArtifactStore, request); err != nil {
		return contracts.Reply{}, err
	}
	a.ledger.recordRequest(request)
	step, ok := a.scenario.Operations[request.Operation]
	if !ok {
		return contracts.Reply{}, fmt.Errorf("%w: %s", ErrUnscripted, request.Operation)
	}
	if step.Effect != "" && artifactEffect[request.Method] != step.Effect {
		return contracts.Reply{}, fmt.Errorf("%w: %s applies no %s", ErrInvalidScenario, request.Method, step.Effect)
	}

	recorded, done, release, err := a.deliveries.claim(ctx, request)
	if err != nil {
		return contracts.Reply{}, err
	}
	if done {
		return recorded, nil
	}
	defer release()

	reply, err := step.Reply.stamp(request.Operation, request.Delivery)
	if err != nil {
		return contracts.Reply{}, err
	}
	if step.Effect != "" {
		if err := a.config.Control.reach(ctx, request.Operation, BeforeEffect); err != nil {
			return contracts.Reply{}, err
		}
		a.ledger.recordEffect(Effect{
			Role: contracts.RoleArtifactStore, Operation: request.Operation, Method: request.Method, Kind: step.Effect,
		})
		a.deliveries.finish(request.Operation, request.Method, reply)
		if err := a.config.Control.reach(ctx, request.Operation, AfterEffect); err != nil {
			return contracts.Reply{}, err
		}
	}
	if step.SendError != nil {
		return contracts.Reply{}, step.SendError
	}
	return reply, nil
}

// Cancel records the cancellation intent. A store answers by reply, so there
// is no scripted cancel failure.
func (a *ArtifactStore) Cancel(ctx context.Context, cancel contracts.Cancel) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return recordCancel(a.ledger, cancel)
}

// Subscribe opens an empty stream: a store answers in replies, not frames,
// and the cursor still has to name a scripted operation.
func (a *ArtifactStore) Subscribe(ctx context.Context, cursor contracts.Cursor) (contracts.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, scripted := a.scenario.Operations[cursor.Operation]
	return subscribeInert(a.config, a.ledger, scripted, cursor)
}
