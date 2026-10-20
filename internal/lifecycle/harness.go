package lifecycle

import (
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

// HarnessMode is how far a runtime harness is trusted for a tenant.
type HarnessMode string

const (
	HarnessOff    HarnessMode = "off"
	HarnessShadow HarnessMode = "shadow"
	HarnessLive   HarnessMode = "live"
)

// HarnessModes lists every HarnessMode.
func HarnessModes() []HarnessMode {
	return []HarnessMode{HarnessOff, HarnessShadow, HarnessLive}
}

// Authoritative reports whether a harness in this mode may change lifecycle
// state or perform governed external effects. Only live may: a shadow harness
// records comparison evidence and nothing else.
func (m HarnessMode) Authoritative() bool {
	return m == HarnessLive
}

// HarnessEvent changes a harness's qualification.
type HarnessEvent string

const (
	HarnessPromote HarnessEvent = "promote"
	HarnessDemote  HarnessEvent = "demote"
	HarnessDisable HarnessEvent = "disable"
)

// HarnessEvents lists every HarnessEvent.
func HarnessEvents() []HarnessEvent {
	return []HarnessEvent{HarnessPromote, HarnessDemote, HarnessDisable}
}

// harnessMachine promotes one step at a time, so a harness is proven in
// shadow before it serves live work. Any mode can be disabled at once.
var harnessMachine = newMachine(HarnessModes(), HarnessEvents(), nil,
	map[HarnessMode]map[HarnessEvent][]HarnessMode{
		HarnessOff:    {HarnessPromote: {HarnessShadow}},
		HarnessShadow: {HarnessPromote: {HarnessLive}, HarnessDemote: {HarnessOff}, HarnessDisable: {HarnessOff}},
		HarnessLive:   {HarnessDemote: {HarnessShadow}, HarnessDisable: {HarnessOff}},
	},
)

// HarnessQualification is a tenant's qualification of one runtime provider.
type HarnessQualification struct {
	Tenant    ids.TenantID   `json:"tenant_id"`
	Provider  ids.ProviderID `json:"provider_id"`
	Mode      HarnessMode    `json:"mode"`
	Version   uint64         `json:"version"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// Validate reports whether the record is complete.
func (h HarnessQualification) Validate() error {
	if h.Tenant.IsZero() || h.Provider.IsZero() || h.Version == 0 || h.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: harness qualification requires identifiers, version and time", ErrInvalid)
	}
	if !slices.Contains(HarnessModes(), h.Mode) {
		return fmt.Errorf("%w: unknown harness mode", ErrInvalid)
	}
	return nil
}

// Apply changes the mode if the record is still at expectedVersion. Promotion
// requires a principal: a harness is never promoted by a provider or by the
// system on its own.
func (h HarnessQualification) Apply(event HarnessEvent, expectedVersion uint64, actor Actor, at time.Time) (HarnessQualification, Change, error) {
	change := Change{
		Tenant: h.Tenant, Record: RecordHarness, RecordID: h.Provider.Untyped(),
		Event: string(event), From: string(h.Mode), Version: h.Version,
	}
	if event == HarnessPromote && actor.Kind != ActorPrincipal {
		return h, change, reject(change, RejectUnauthorizedActor)
	}
	next, change, err := transition(harnessMachine, change, h.Mode, h.Version, expectedVersion, event, "")
	if err != nil {
		return h, change, err
	}
	h.Mode, h.Version, h.UpdatedAt = next, change.Version, at
	return h, change, nil
}

// MigrationState is the state of a pass moving work between harnesses.
type MigrationState string

const (
	MigrationPlanned   MigrationState = "planned"
	MigrationPreviewed MigrationState = "previewed"
	MigrationHeld      MigrationState = "held"
	MigrationExecuting MigrationState = "executing"
	MigrationCompleted MigrationState = "completed"
	MigrationFailed    MigrationState = "failed"
	MigrationCancelled MigrationState = "cancelled"
)

// MigrationStates lists every MigrationState.
func MigrationStates() []MigrationState {
	return []MigrationState{
		MigrationPlanned, MigrationPreviewed, MigrationHeld, MigrationExecuting,
		MigrationCompleted, MigrationFailed, MigrationCancelled,
	}
}

// MigrationEvent is a command or outcome for a migration pass.
type MigrationEvent string

const (
	MigrationPreview  MigrationEvent = "preview"
	MigrationHold     MigrationEvent = "hold"
	MigrationRelease  MigrationEvent = "release"
	MigrationExecute  MigrationEvent = "execute"
	MigrationComplete MigrationEvent = "complete"
	MigrationFail     MigrationEvent = "fail"
	MigrationCancel   MigrationEvent = "cancel"
)

// MigrationEvents lists every MigrationEvent.
func MigrationEvents() []MigrationEvent {
	return []MigrationEvent{
		MigrationPreview, MigrationHold, MigrationRelease, MigrationExecute,
		MigrationComplete, MigrationFail, MigrationCancel,
	}
}

// commanded are the migration events only a principal may issue.
var commanded = []MigrationEvent{MigrationPreview, MigrationHold, MigrationRelease, MigrationExecute, MigrationCancel}

// migrationMachine allows execution only from a preview. Releasing a hold
// returns to planned, so execution after a hold needs a fresh preview.
var migrationMachine = newMachine(MigrationStates(), MigrationEvents(),
	[]MigrationState{MigrationCompleted, MigrationFailed, MigrationCancelled},
	map[MigrationState]map[MigrationEvent][]MigrationState{
		MigrationPlanned: {
			MigrationPreview: {MigrationPreviewed},
			MigrationHold:    {MigrationHeld},
			MigrationCancel:  {MigrationCancelled},
		},
		MigrationPreviewed: {
			MigrationPreview: {MigrationPreviewed},
			MigrationHold:    {MigrationHeld},
			MigrationExecute: {MigrationExecuting},
			MigrationCancel:  {MigrationCancelled},
		},
		MigrationHeld: {
			MigrationRelease: {MigrationPlanned},
			MigrationCancel:  {MigrationCancelled},
		},
		MigrationExecuting: {
			MigrationComplete: {MigrationCompleted},
			MigrationFail:     {MigrationFailed},
		},
	},
)

// MigrationPass moves a tenant's work from one runtime harness to another. It
// is durable and every command is audited.
type MigrationPass struct {
	ID        ids.MigrationID `json:"migration_id"`
	Tenant    ids.TenantID    `json:"tenant_id"`
	From      ids.ProviderID  `json:"from_provider_id"`
	To        ids.ProviderID  `json:"to_provider_id"`
	State     MigrationState  `json:"state"`
	Version   uint64          `json:"version"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// MigrationItemState is the durable per-Attempt outcome of a tenant-wide
// harness migration. A pass may complete only after every item completed;
// failed and unresolved items deliberately keep the pass non-successful.
type MigrationItemState string

const (
	MigrationItemQueued     MigrationItemState = "queued"
	MigrationItemQuiescing  MigrationItemState = "quiescing"
	MigrationItemStarting   MigrationItemState = "starting"
	MigrationItemCutover    MigrationItemState = "cutover"
	MigrationItemCleaningUp MigrationItemState = "cleaning_up"
	MigrationItemCompleted  MigrationItemState = "completed"
	MigrationItemFailed     MigrationItemState = "failed"
	MigrationItemUnresolved MigrationItemState = "unresolved"
)

// MigrationItemStates lists every durable migration item state.
func MigrationItemStates() []MigrationItemState {
	return []MigrationItemState{
		MigrationItemQueued, MigrationItemQuiescing, MigrationItemStarting,
		MigrationItemCutover, MigrationItemCleaningUp, MigrationItemCompleted,
		MigrationItemFailed, MigrationItemUnresolved,
	}
}

// MigrationItem is one source Attempt captured when a tenant-wide pass
// starts. TargetAttempt is allocated only after source quiescence and means
// restart-from-artifacts; it never reuses a native source session.
type MigrationItem struct {
	Tenant               ids.TenantID       `json:"tenant_id"`
	Migration            ids.MigrationID    `json:"migration_id"`
	SourceAttempt        ids.AttemptID      `json:"source_attempt_id"`
	TargetAttempt        ids.AttemptID      `json:"target_attempt_id,omitzero"`
	QuiesceOperation     ids.OperationID    `json:"quiesce_operation_id,omitzero"`
	EnvironmentOperation ids.OperationID    `json:"environment_operation_id,omitzero"`
	RestartOperation     ids.OperationID    `json:"restart_operation_id,omitzero"`
	CleanupOperation     ids.OperationID    `json:"cleanup_operation_id,omitzero"`
	ContextRefs          []string           `json:"context_refs,omitempty"`
	EnvironmentRef       string             `json:"environment_ref,omitempty"`
	Reason               string             `json:"reason,omitempty"`
	State                MigrationItemState `json:"state"`
	ClaimEpoch           uint64             `json:"claim_epoch"`
	QuiesceEpoch         uint64             `json:"quiesce_epoch,omitzero"`
	EnvironmentEpoch     uint64             `json:"environment_epoch,omitzero"`
	RestartEpoch         uint64             `json:"restart_epoch,omitzero"`
	CleanupEpoch         uint64             `json:"cleanup_epoch,omitzero"`
	UpdatedAt            time.Time          `json:"updated_at"`
}

// Validate reports whether an item can be used as durable migration evidence.
func (i MigrationItem) Validate() error {
	if i.Tenant.IsZero() || i.Migration.IsZero() || i.SourceAttempt.IsZero() || i.ClaimEpoch == 0 || i.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: migration item requires tenant, pass, source, fence and time", ErrInvalid)
	}
	if !slices.Contains(MigrationItemStates(), i.State) {
		return fmt.Errorf("%w: unknown migration item state", ErrInvalid)
	}
	if i.State != MigrationItemQueued && i.State != MigrationItemUnresolved && i.QuiesceOperation.IsZero() {
		return fmt.Errorf("%w: active migration item requires a quiesce operation", ErrInvalid)
	}
	if (i.State == MigrationItemUnresolved) != (i.Reason != "") || len(i.Reason) > 128 {
		return fmt.Errorf("%w: unresolved migration item requires a bounded reason", ErrInvalid)
	}
	for _, stage := range []struct {
		operation ids.OperationID
		epoch     uint64
	}{{i.QuiesceOperation, i.QuiesceEpoch}, {i.EnvironmentOperation, i.EnvironmentEpoch}, {i.RestartOperation, i.RestartEpoch}, {i.CleanupOperation, i.CleanupEpoch}} {
		if stage.epoch > i.ClaimEpoch || stage.operation.IsZero() && stage.epoch != 0 {
			return fmt.Errorf("%w: migration stage fence is invalid", ErrInvalid)
		}
	}
	if i.TargetAttempt.IsZero() && (i.State == MigrationItemStarting || i.State == MigrationItemCutover || i.State == MigrationItemCleaningUp || i.State == MigrationItemCompleted) {
		return fmt.Errorf("%w: cut over migration item requires a target attempt", ErrInvalid)
	}
	if !i.TargetAttempt.IsZero() && i.RestartOperation.IsZero() {
		return fmt.Errorf("%w: target migration item requires a restart operation", ErrInvalid)
	}
	if !i.TargetAttempt.IsZero() && i.EnvironmentOperation.IsZero() {
		return fmt.Errorf("%w: target migration requires an environment operation", ErrInvalid)
	}
	if i.EnvironmentRef != "" && !migrationContextReference.MatchString(i.EnvironmentRef) {
		return fmt.Errorf("%w: migration environment reference is invalid", ErrInvalid)
	}
	if (i.State == MigrationItemCleaningUp || i.State == MigrationItemCompleted) && i.CleanupOperation.IsZero() {
		return fmt.Errorf("%w: cleanup migration item requires a cleanup operation", ErrInvalid)
	}
	if i.State == MigrationItemCompleted && (i.QuiesceEpoch == 0 || i.EnvironmentEpoch == 0 || i.RestartEpoch == 0 || i.CleanupEpoch == 0) {
		return fmt.Errorf("%w: completed migration requires stage fences", ErrInvalid)
	}
	if !i.TargetAttempt.IsZero() && i.TargetAttempt == i.SourceAttempt {
		return fmt.Errorf("%w: migration target must be a new attempt", ErrInvalid)
	}
	if len(i.ContextRefs) > 32 {
		return fmt.Errorf("%w: migration has too many context references", ErrInvalid)
	}
	for _, ref := range i.ContextRefs {
		if !migrationContextReference.MatchString(ref) {
			return fmt.Errorf("%w: migration context reference is invalid", ErrInvalid)
		}
	}
	return nil
}

var migrationContextReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)

// NewMigrationPass returns a planned pass at version 1.
func NewMigrationPass(id ids.MigrationID, tenant ids.TenantID, from, to ids.ProviderID, at time.Time) (MigrationPass, error) {
	pass := MigrationPass{ID: id, Tenant: tenant, From: from, To: to, State: MigrationPlanned, Version: 1, CreatedAt: at, UpdatedAt: at}
	return pass, pass.Validate()
}

// Validate reports whether the record is complete.
func (p MigrationPass) Validate() error {
	if p.ID.IsZero() || p.Tenant.IsZero() || p.From.IsZero() || p.To.IsZero() || p.Version == 0 {
		return fmt.Errorf("%w: migration pass requires identifiers and a positive version", ErrInvalid)
	}
	if p.From == p.To {
		return fmt.Errorf("%w: migration pass must change harness", ErrInvalid)
	}
	if !slices.Contains(MigrationStates(), p.State) {
		return fmt.Errorf("%w: unknown migration state", ErrInvalid)
	}
	if p.CreatedAt.IsZero() || p.UpdatedAt.Before(p.CreatedAt) {
		return fmt.Errorf("%w: migration pass timestamps are inconsistent", ErrInvalid)
	}
	return nil
}

// Apply performs event if the pass is still at expectedVersion. Commands
// (preview, hold, release, execute, cancel) require a principal actor; the
// outcomes complete and fail are reported by the system.
func (p MigrationPass) Apply(event MigrationEvent, expectedVersion uint64, actor Actor, at time.Time) (MigrationPass, Change, error) {
	change := Change{
		Tenant: p.Tenant, Record: RecordMigration, RecordID: p.ID.Untyped(),
		Event: string(event), From: string(p.State), Version: p.Version,
	}
	if slices.Contains(commanded, event) != (actor.Kind == ActorPrincipal) {
		return p, change, reject(change, RejectUnauthorizedActor)
	}
	next, change, err := transition(migrationMachine, change, p.State, p.Version, expectedVersion, event, "")
	if err != nil {
		return p, change, err
	}
	p.State, p.Version, p.UpdatedAt = next, change.Version, at
	return p, change, nil
}
