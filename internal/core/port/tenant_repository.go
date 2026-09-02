package port

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
)

// TenantRepository owns reads/writes on the tenants row for the current
// RLS-scoped tenant. Under normal calls RLS restricts visibility to the
// caller's tenant (the tenants policy uses `id = current_setting`, so
// FindByID always returns the caller's own row when it exists).
type TenantRepository interface {
	// FindByID returns the tenant row matching id. If RLS blocks the row
	// (caller is not the same tenant), returns domain.ErrTenantNotFound
	// wrapped in a DomainError. Filters deleted_at IS NULL — offboarded
	// (soft-deleted) tenants return ErrTenantNotFound.
	FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)

	// FindByIDIncludingDeleted returns the tenant row regardless of deleted_at.
	// Used only by iam-system internal paths (I-2 RP cleanup, reconcilers) that
	// must act on offboarded tenants after O&M soft-deletes them. Never called
	// from public/operator routes.
	FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)

	// Update applies patch under optimistic-locking (CONC-1..4) inside the
	// caller's transaction. On WHERE record_version mismatch, returns
	// ErrOptimisticLockConflict wrapped in DomainError with Details
	// containing the current record_version + updated_at (CONC-4).
	Update(ctx context.Context, id uuid.UUID, patch *domain.TenantPatch) (*domain.Tenant, error)

	// SetRealmSyncPending marks the tenant row realm_sync_pending=true so the
	// realm-config-sync reconciler picks it up (T-15 Option A, §16 A58).
	// Called by TenantService.Patch when RP.PatchRealmConfig fails after a
	// successful local UPDATE — the committed local change must be reconciled.
	// Idempotent; no optimistic-lock (setting the flag is safe to repeat).
	SetRealmSyncPending(ctx context.Context, tenantID uuid.UUID) error

	// Insert creates a new tenant row (used by I-1 in Phase 4 and by the
	// TrialTenantProvisioned consumer in Phase 3). Phase 2 exposes it for
	// admin-side seeding paths only.
	//
	// LLD I-1 idempotency: on ON CONFLICT (id) DO NOTHING the row already
	// existed; the returned bool wasCreated=false signals the caller should
	// skip re-seeding and respond 200 (idempotent replay) instead of 201.
	// wasCreated=true means the row was freshly inserted this call.
	Insert(ctx context.Context, t *domain.Tenant) (*domain.Tenant, bool, error)

	// ListSubscriptionLapses returns every tenant with status='cancelled'
	// whose cancelled_at is at least graceDays in the past — I-16 (§16
	// RP-C3), the bulk-read RP's subscription-lapse sweep polls instead of
	// tracking cancellation timestamps itself. graceDays is O&M's own
	// SUBSCRIPTION_GRACE_DAYS config; O&M does the grace-period math so the
	// rule stays single-sourced instead of drifting between two services.
	//
	// Cross-tenant by design: this is meaningless against the RLS-scoped
	// app pool (RLS would silently narrow it to at most the caller's own
	// row) — callers MUST construct this repository against a BYPASSRLS
	// pool (org_membership_migrator / sysPool), exactly like the reconciler
	// jobs and business-metric exporters already do.
	ListSubscriptionLapses(ctx context.Context, graceDays int) ([]domain.Tenant, error)

	// LockByID takes TM-13 SELECT … FOR UPDATE on the tenants row so
	// last-owner / seat-cap mutations serialize. No-op (no error) when the
	// row is missing — callers discover that via the subsequent membership
	// / invitation lookup.
	LockByID(ctx context.Context, id uuid.UUID) error

	// LicensedSeatsForUpdate locks the tenants row (TM-13) and returns
	// licensed_seats. Used by SEAT-1 invite so concurrent Invite calls
	// cannot both slip past a stale under-cap count.
	LicensedSeatsForUpdate(ctx context.Context, id uuid.UUID) (int, error)

	// SetFeatureFlags is O-4: UPDATE feature_flags under optimistic lock.
	// Zero rows → ErrTenantNotFound or ErrOptimisticLockConflict.
	SetFeatureFlags(ctx context.Context, id uuid.UUID, flags []byte, expectedVersion int64) error

	// ClearOwnerlessSince is O-7: owner reassignment clears the TM-12 flag.
	ClearOwnerlessSince(ctx context.Context, id uuid.UUID) error

	// MarkOwnerlessIfUnset is TM-12 / I-5: sets ownerless_since = now()
	// only when currently NULL. Returns true when this call flipped the flag.
	MarkOwnerlessIfUnset(ctx context.Context, id uuid.UUID) (bool, error)

	// SetRealmFields is I-2: RP writes realm_id / realm_type / keycloak_shard
	// under optimistic lock, including offboarded (deleted_at IS NOT NULL) rows.
	SetRealmFields(ctx context.Context, id uuid.UUID, realmID string, realmType domain.RealmType, shard string, recordVersion int64) error

	// LockSeatOccupancy is SEAT-5: FOR UPDATE the tenants row then count
	// active memberships + unexpired pending invitations.
	LockSeatOccupancy(ctx context.Context, id uuid.UUID) (SeatOccupancy, error)

	// SetOverageSince writes or clears tenants.overage_since. nil clears.
	SetOverageSince(ctx context.Context, id uuid.UUID, since *time.Time) error

	// LockForProjection takes SELECT … FOR UPDATE on the live tenants row
	// for EVT-14. Missing / soft-deleted row returns (nil, nil).
	LockForProjection(ctx context.Context, id uuid.UUID) (*TenantProjectionLock, error)

	// SetLastEventAt is the EVT-14 watermark write after a projection runs.
	SetLastEventAt(ctx context.Context, id uuid.UUID, t time.Time) error

	// ApplyLifecyclePatch applies one consumer projection UPDATE. rows is
	// RowsAffected — TrialReactivated and TenantReactivated branch on it.
	ApplyLifecyclePatch(ctx context.Context, id uuid.UUID, patch TenantLifecyclePatch) (rows int64, err error)

	// WipeTenantChildren deletes Core's tenant-scoped child rows on
	// offboard (GDPR wipe). The tenants row itself stays soft-deleted.
	WipeTenantChildren(ctx context.Context, id uuid.UUID) error
}

// TenantProjectionLock is the EVT-14 snapshot under FOR UPDATE.
type TenantProjectionLock struct {
	Status      domain.SubscriptionStatus
	Plan        domain.TenantPlan
	LastEventAt *time.Time
}

// TenantLifecycleOp selects which projection UPDATE ApplyLifecyclePatch runs.
type TenantLifecycleOp int

const (
	LifecycleSetRealm TenantLifecycleOp = iota + 1
	LifecycleActivatePaid
	LifecycleSetStatusClearSuspension
	LifecycleTrialReactivate
	LifecycleSuspendBillingLapse
	LifecycleSuspendOperator
	LifecycleOffboard
	LifecycleSetPlan
	LifecycleCancel
	LifecycleReactivatePaid
	LifecycleReactivateTrial
	LifecycleSetLicensedSeats
)

// TenantLifecyclePatch is one consumer projection UPDATE.
type TenantLifecyclePatch struct {
	Op                TenantLifecycleOp
	RealmID           string
	RealmType         string
	KeycloakShard     string
	Plan              domain.TenantPlan
	Status            domain.SubscriptionStatus
	TrialDurationDays int
	LicensedSeats     int
}

// SeatOccupancy is the locked SEAT-5 snapshot for one tenant.
type SeatOccupancy struct {
	LicensedSeats int
	OverageSince  *time.Time
	Active        int
	Pending       int
}

// TenantRepositoryNoop is a compile-time stub for tests that only override
// a subset of TenantRepository. Embed it so new methods don't break fakes.
type TenantRepositoryNoop struct{}

func (TenantRepositoryNoop) FindByID(context.Context, uuid.UUID) (*domain.Tenant, error) {
	return nil, nil
}
func (TenantRepositoryNoop) FindByIDIncludingDeleted(context.Context, uuid.UUID) (*domain.Tenant, error) {
	return nil, nil
}
func (TenantRepositoryNoop) Update(context.Context, uuid.UUID, *domain.TenantPatch) (*domain.Tenant, error) {
	return nil, nil
}
func (TenantRepositoryNoop) SetRealmSyncPending(context.Context, uuid.UUID) error { return nil }
func (TenantRepositoryNoop) Insert(context.Context, *domain.Tenant) (*domain.Tenant, bool, error) {
	return nil, false, nil
}
func (TenantRepositoryNoop) ListSubscriptionLapses(context.Context, int) ([]domain.Tenant, error) {
	return nil, nil
}
func (TenantRepositoryNoop) LockByID(context.Context, uuid.UUID) error { return nil }
func (TenantRepositoryNoop) LicensedSeatsForUpdate(context.Context, uuid.UUID) (int, error) {
	return 0, nil
}
func (TenantRepositoryNoop) SetFeatureFlags(context.Context, uuid.UUID, []byte, int64) error {
	return nil
}
func (TenantRepositoryNoop) ClearOwnerlessSince(context.Context, uuid.UUID) error { return nil }
func (TenantRepositoryNoop) MarkOwnerlessIfUnset(context.Context, uuid.UUID) (bool, error) {
	return false, nil
}
func (TenantRepositoryNoop) SetRealmFields(context.Context, uuid.UUID, string, domain.RealmType, string, int64) error {
	return nil
}
func (TenantRepositoryNoop) LockSeatOccupancy(context.Context, uuid.UUID) (SeatOccupancy, error) {
	return SeatOccupancy{}, nil
}
func (TenantRepositoryNoop) SetOverageSince(context.Context, uuid.UUID, *time.Time) error {
	return nil
}
func (TenantRepositoryNoop) LockForProjection(context.Context, uuid.UUID) (*TenantProjectionLock, error) {
	return nil, nil
}
func (TenantRepositoryNoop) SetLastEventAt(context.Context, uuid.UUID, time.Time) error { return nil }
func (TenantRepositoryNoop) ApplyLifecyclePatch(context.Context, uuid.UUID, TenantLifecyclePatch) (int64, error) {
	return 0, nil
}
func (TenantRepositoryNoop) WipeTenantChildren(context.Context, uuid.UUID) error { return nil }

var _ TenantRepository = TenantRepositoryNoop{}
