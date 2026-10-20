package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
	"github.com/AssahBismarkabah/dac-plane/internal/lifecycle"
	store "github.com/AssahBismarkabah/dac-plane/internal/platform/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

const migrationPreviewTTL = 5 * time.Minute

// CreateMigrationPass records administrative intent, never provider transfer.
type CreateMigrationPass struct {
	Command
	From, To ids.ProviderID
}

// MigrationAdministration names only principal-controlled migration events.
// There is deliberately no completion or failure outcome command: #60 owns
// the future execution engine and its evidence requirements.
type MigrationAdministration struct {
	Command
	Migration ids.MigrationID
	Event     lifecycle.MigrationEvent
}

// MigrationView returns durable administration, not a migrated binding.
type MigrationView struct {
	Pass    lifecycle.MigrationPass
	Preview store.MigrationPreview
	Claim   ids.ClaimID
}

func authorizeMigration(ctx context.Context, u *store.UnitOfWork, actor identity.AuthorizationContext) error {
	p := actor.Principal()
	allowed, err := store.LockHarnessAdministrator(ctx, u, p.Tenant, p.ID)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrUnauthorized
	}
	return nil
}

func migrationNow(ctx context.Context, u *unit) error {
	var now time.Time
	if err := u.uow.QueryRow(ctx, `select clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	u.at = now.UTC().Truncate(time.Microsecond)
	return nil
}

// CreateMigration allocates a tenant-wide administrative pass and reusable
// claim under current administrator permission. It makes no provider calls.
func CreateMigration(ctx context.Context, pool *pgxpool.Pool, cmd CreateMigrationPass) (MigrationView, error) {
	if err := cmd.Command.validate(true); err != nil {
		return MigrationView{}, err
	}
	if cmd.From.IsZero() || cmd.To.IsZero() || cmd.From == cmd.To {
		return MigrationView{}, ErrInvalidCommand
	}
	digest, err := fingerprint("create_migration", struct{ From, To ids.ProviderID }{cmd.From, cmd.To})
	if err != nil {
		return MigrationView{}, err
	}
	result, err := executeAuthorized(ctx, pool, cmd.Command, "create_migration", digest, time.Now().UTC(), func(u *store.UnitOfWork) error { return authorizeMigration(ctx, u, cmd.Actor) }, func(u *unit) (json.RawMessage, error) {
		tenant := cmd.Actor.Principal().Tenant
		if err := store.LockMigrationAdministration(ctx, u.uow, tenant); err != nil {
			return nil, err
		}
		slot, err := store.GetMigrationSlot(ctx, u.uow, tenant)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if !slot.Active.IsZero() {
			return nil, fmt.Errorf("%w: tenant already has a migration pass", ErrInvalidCommand)
		}
		id, err := ids.New[ids.Migration]()
		if err != nil {
			return nil, err
		}
		if err := migrationNow(ctx, u); err != nil {
			return nil, err
		}
		pass, err := lifecycle.NewMigrationPass(id, tenant, cmd.From, cmd.To, u.at)
		if err != nil {
			return nil, err
		}
		a, b, err := store.LockMigrationQualifications(ctx, u.uow, pass)
		if errors.Is(err, store.ErrNotFound) || err == nil && (!a.Mode.Authoritative() || !b.Mode.Authoritative()) {
			return nil, ErrNoHarness
		}
		if err != nil {
			return nil, err
		}
		if err := migrationNow(ctx, u); err != nil {
			return nil, err
		}
		pass.CreatedAt, pass.UpdatedAt = u.at, u.at
		tr, err := u.transition(0, nil)
		if err != nil {
			return nil, err
		}
		if err := store.InsertAdminMigration(ctx, u.uow, pass, tr); err != nil {
			return nil, err
		}
		if slot.Claim.IsZero() {
			claimID, err := ids.New[ids.Claim]()
			if err != nil {
				return nil, err
			}
			claim, err := lifecycle.NewMaintenanceClaim(claimID, tenant, cmd.From, "tenant-harness-administration", u.at)
			if err != nil {
				return nil, err
			}
			if _, _, err := store.InsertClaim(ctx, u.uow, claim); err != nil {
				return nil, err
			}
			slot.Claim = claimID
		}
		slot.Active = id
		if err := store.SetMigrationSlot(ctx, u.uow, tenant, slot); err != nil {
			return nil, err
		}
		return json.Marshal(MigrationView{Pass: pass, Claim: slot.Claim})
	})
	var view MigrationView
	if err == nil {
		err = json.Unmarshal(result, &view)
	}
	return view, err
}

// AdministerMigration applies one versioned administrative event. Exact nonce
// replay returns the original view, but still requires current permission.
func AdministerMigration(ctx context.Context, pool *pgxpool.Pool, cmd MigrationAdministration) (MigrationView, error) {
	if err := cmd.Command.validate(false); err != nil {
		return MigrationView{}, err
	}
	if cmd.Migration.IsZero() {
		return MigrationView{}, ErrInvalidCommand
	}
	switch cmd.Event {
	case lifecycle.MigrationPreview, lifecycle.MigrationHold, lifecycle.MigrationRelease, lifecycle.MigrationExecute, lifecycle.MigrationCancel:
	default:
		return MigrationView{}, ErrInvalidCommand
	}
	digest, err := fingerprint("administer_migration", struct {
		Migration ids.MigrationID
		Event     lifecycle.MigrationEvent
		Version   uint64
	}{cmd.Migration, cmd.Event, cmd.ExpectedVersion})
	if err != nil {
		return MigrationView{}, err
	}
	result, err := executeAuthorized(ctx, pool, cmd.Command, "administer_migration", digest, time.Now().UTC(), func(u *store.UnitOfWork) error { return authorizeMigration(ctx, u, cmd.Actor) }, func(u *unit) (json.RawMessage, error) {
		tenant := cmd.Actor.Principal().Tenant
		if err := store.LockMigrationAdministration(ctx, u.uow, tenant); err != nil {
			return nil, err
		}
		slot, err := store.GetMigrationSlot(ctx, u.uow, tenant)
		if err != nil {
			return nil, err
		}
		if slot.Active != cmd.Migration {
			return nil, ErrNotFound
		}
		pass, err := store.GetMigrationPassForUpdate(ctx, u.uow, tenant, cmd.Migration)
		if err != nil {
			return nil, err
		}
		preview, err := store.GetMigrationPreview(ctx, u.uow, tenant, pass.ID)
		if err != nil {
			return nil, err
		}
		if cmd.Event == lifecycle.MigrationPreview || cmd.Event == lifecycle.MigrationExecute {
			a, b, err := store.LockMigrationQualifications(ctx, u.uow, pass)
			if errors.Is(err, store.ErrNotFound) || err == nil && (!a.Mode.Authoritative() || !b.Mode.Authoritative()) {
				return nil, ErrNoHarness
			}
			if err != nil {
				return nil, err
			}
			if err := migrationNow(ctx, u); err != nil {
				return nil, err
			}
			if cmd.Event == lifecycle.MigrationExecute {
				if preview.At.IsZero() || !u.at.Before(preview.At.Add(migrationPreviewTTL)) || preview.FromVersion != a.Version || preview.ToVersion != b.Version {
					return nil, fmt.Errorf("%w: migration preview is stale", ErrInvalidCommand)
				}
			} else {
				preview = store.MigrationPreview{FromVersion: a.Version, ToVersion: b.Version, At: u.at}
			}
		} else {
			if err := migrationNow(ctx, u); err != nil {
				return nil, err
			}
			preview = store.MigrationPreview{}
		}
		tr, err := u.transition(cmd.ExpectedVersion, nil)
		if err != nil {
			return nil, err
		}
		pass, err = store.ApplyMigrationPass(ctx, u.uow, tenant, pass.ID, cmd.Event, tr)
		if err != nil {
			return nil, err
		}
		if err := store.SetMigrationPreview(ctx, u.uow, tenant, pass.ID, preview); err != nil {
			return nil, err
		}
		if cmd.Event == lifecycle.MigrationCancel {
			slot.Active = ids.MigrationID{}
			if err := store.SetMigrationSlot(ctx, u.uow, tenant, slot); err != nil {
				return nil, err
			}
		}
		return json.Marshal(MigrationView{Pass: pass, Preview: preview, Claim: slot.Claim})
	})
	var view MigrationView
	if err == nil {
		err = json.Unmarshal(result, &view)
	}
	return view, err
}

// GetMigration requires current administrator permission even for old passes.
func GetMigration(ctx context.Context, pool *pgxpool.Pool, actor identity.AuthorizationContext, id ids.MigrationID) (MigrationView, error) {
	var view MigrationView
	p := actor.Principal()
	if p.Validate() != nil || id.IsZero() {
		return view, ErrInvalidCommand
	}
	u, err := store.Begin(ctx, pool)
	if err != nil {
		return view, err
	}
	defer func() { _ = u.Rollback(ctx) }()
	stored, err := store.GetPrincipal(ctx, u, p.Tenant, p.ID)
	if errors.Is(err, store.ErrNotFound) || err == nil && stored != p {
		return view, ErrUnauthorized
	}
	if err != nil {
		return view, err
	}
	if err := authorizeMigration(ctx, u, actor); err != nil {
		return view, err
	}
	if err := store.LockMigrationAdministration(ctx, u, p.Tenant); err != nil {
		return view, err
	}
	view.Pass, err = store.GetMigrationPass(ctx, u, p.Tenant, id)
	if err != nil {
		return view, err
	}
	view.Preview, err = store.GetMigrationPreview(ctx, u, p.Tenant, id)
	if errors.Is(err, store.ErrNotFound) {
		// Pre-administration passes remain readable, but gain neither a
		// fabricated preview nor a claim belonging to a different pass.
		return view, u.Commit(ctx)
	}
	if err != nil {
		return view, err
	}
	slot, err := store.GetMigrationSlot(ctx, u, p.Tenant)
	if err != nil {
		return view, err
	}
	view.Claim = slot.Claim
	return view, u.Commit(ctx)
}
