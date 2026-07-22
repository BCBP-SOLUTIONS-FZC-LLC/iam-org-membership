package service

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ProvisioningService owns I-1 (tenant creation), I-2 (RP realm patch),
// I-4 (Keycloak lifecycle status), I-5 (delete cascade). Called by Realm
// Provisioner + Signup BFF + Event Consumer over the /internal/* mesh.
type ProvisioningService struct {
	pool        *pgcommon.Pool
	tenants     port.TenantRepository
	memberships port.MembershipRepository
	roles       port.TenantRoleRepository
	deptMems    port.DeptMembershipRepository
	labels      port.DeptRoleLabelRepository
	tenantDepts port.TenantDepartmentRepository
	depts       port.DepartmentRepository
	delegations port.DelegationRepository
	acls        port.TenderACLRepository
	plans       port.PlanRepository
	txRunner    port.TxRunner
	cache       port.Cache
	rp          port.RealmProvisionerClient
}

func NewProvisioningService(
	pool *pgcommon.Pool,
	tenants port.TenantRepository, memberships port.MembershipRepository,
	roles port.TenantRoleRepository, deptMems port.DeptMembershipRepository,
	labels port.DeptRoleLabelRepository, tenantDepts port.TenantDepartmentRepository,
	depts port.DepartmentRepository, delegations port.DelegationRepository,
	acls port.TenderACLRepository, plans port.PlanRepository,
	txRunner port.TxRunner, cache port.Cache, rp port.RealmProvisionerClient,
) *ProvisioningService {
	return &ProvisioningService{
		pool: pool, tenants: tenants, memberships: memberships,
		roles: roles, deptMems: deptMems, labels: labels,
		tenantDepts: tenantDepts, depts: depts,
		delegations: delegations, acls: acls, plans: plans,
		txRunner: txRunner, cache: cache, rp: rp,
	}
}

// TrialSignupInput carries the I-1 payload from Realm Provisioner /
// Signup BFF after the shared trial-realm user has been created (§8.1).
type TrialSignupInput struct {
	TenantID      uuid.UUID
	Slug          string
	Name          string
	Plan          domain.TenantPlan
	OwnerUserID   uuid.UUID
	DefaultLocale string
}

// TrialSignup is I-1: creates the tenant row + 5 system dept activations
// + 3 role labels + owner membership + tenant_owner role grant, and emits
// TenantCreated + TrialStarted in one RunInTx (§8.1). Runs under GUCSet
// (target tenant + iam-system principal, RLS-5) so the RLS WITH CHECK
// passes on inserts into tenant-scoped tables.
func (s *ProvisioningService) TrialSignup(ctx context.Context, req TrialSignupInput) (*domain.Tenant, error) {
	if req.Plan == "" {
		req.Plan = domain.PlanStarter
	}
	// Look up plan for trial_duration_days.
	plan, err := s.plans.FindByCode(ctx, req.Plan)
	if err != nil {
		return nil, err
	}
	trialEnds := time.Now().UTC().Add(time.Duration(plan.TrialDurationDays) * 24 * time.Hour)

	// Set GUC to the new tenant + iam-system so RLS WITH CHECK passes on
	// inserts. This is the RLS-5 internal-provisioning path.
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = "iam-system"
	g.TenantID = req.TenantID.String()
	gucCtx := pgcommon.WithGUCSet(ctx, g)

	locale := req.DefaultLocale
	if locale == "" {
		locale = "en-US"
	}

	var created *domain.Tenant
	err = s.txRunner.RunInTx(gucCtx, func(txCtx context.Context) error {
		// 1) Create the tenant row.
		t, err := s.tenants.Insert(txCtx, &domain.Tenant{
			ID:                   req.TenantID,
			Slug:                 req.Slug,
			Name:                 req.Name,
			Plan:                 req.Plan,
			Status:               domain.StatusTrial,
			TrialEndsAt:          &trialEnds,
			RealmID:              "trial",
			RealmType:            domain.RealmShared,
			KeycloakShard:        "shard-0",
			MFAFreshnessSeconds:  300,
			LocalAccountsEnabled: true,
			DefaultLocale:        locale,
			LicensedSeats:        10,
		})
		if err != nil {
			return err
		}
		created = t

		// 2) Activate 5 system departments (§8.1). Use ListActive from
		// catalog — filter is_system=true, is_active=true.
		catalog, err := s.depts.List(txCtx, true)
		if err != nil {
			return err
		}
		for _, d := range catalog {
			if !d.IsSystem {
				continue
			}
			if _, err := s.tenantDepts.Activate(txCtx, req.TenantID, d.ID); err != nil {
				return err
			}
		}

		// 3) Seed 3 dept-role labels (DRL-1).
		if _, err := s.labels.Seed(txCtx, req.TenantID); err != nil {
			return err
		}

		// 4) Owner membership.
		mem, err := s.memberships.Insert(txCtx, &domain.TenantMembership{
			TenantID: req.TenantID,
			UserID:   req.OwnerUserID,
			Status:   domain.MembershipActive,
		})
		if err != nil {
			return err
		}

		// 5) Grant tenant_owner role.
		if _, err := s.roles.Grant(txCtx, &domain.TenantRole{
			TenantID:           req.TenantID,
			UserID:             req.OwnerUserID,
			TenantMembershipID: mem.ID,
			RoleCode:           domain.RoleTenantOwner,
			GrantedBy:          req.OwnerUserID,
		}); err != nil {
			return err
		}

		// 6) Emit TenantCreated + TrialStarted (both on iam.tenant.events
		// via RoutingPublisher).
		pub, _ := port.EventPublisherFromContext(txCtx)
		if pub != nil {
			_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
				Type: domain.EventTenantCreated, TenantID: req.TenantID,
				Subject: req.TenantID.String(), Actor: "iam-system",
				Data: domain.TenantCreatedPayload{
					TenantID: req.TenantID, Slug: req.Slug, Plan: req.Plan, Status: domain.StatusTrial,
				},
			})
			_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
				Type: domain.EventTrialStarted, TenantID: req.TenantID,
				Subject: req.TenantID.String(), Actor: "iam-system",
				Data: domain.TrialStartedPayload{
					TenantID: req.TenantID, Plan: req.Plan, TrialEndsAt: trialEnds,
				},
			})
			// Also emit TenantRoleGranted for the owner (§16 A14).
			_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
				Type: domain.EventTenantRoleGranted, TenantID: req.TenantID,
				Subject: req.OwnerUserID.String(), Actor: "iam-system",
				Data: domain.TenantRoleGrantedPayload{
					UserID: req.OwnerUserID, TenantID: req.TenantID,
					RoleCode: domain.RoleTenantOwner, ActorID: uuid.Nil,
				},
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// SetRealmFields is I-2: RP updates realm_id/realm_type/keycloak_shard
// atomically after dedicated-realm provisioning (TenantConverted flow).
func (s *ProvisioningService) SetRealmFields(ctx context.Context, tenantID uuid.UUID, realmID string, realmType domain.RealmType, shard string) error {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = "iam-system"
	g.TenantID = tenantID.String()
	gucCtx := pgcommon.WithGUCSet(ctx, g)
	return pgcommon.RunInTx(gucCtx, s.pool, pgx.TxOptions{}, func(txCtx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(txCtx,
			`UPDATE tenants SET realm_id = $2, realm_type = $3, keycloak_shard = $4 WHERE id = $1 AND deleted_at IS NULL`,
			tenantID, realmID, string(realmType), shard)
		return err
	})
}

// SetMembershipStatus is I-4: Event Consumer updates lifecycle status.
func (s *ProvisioningService) SetMembershipStatus(ctx context.Context, tenantID, userID uuid.UUID, status domain.MembershipStatus, expectedVersion int64) (*domain.TenantMembership, error) {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = "iam-system"
	g.TenantID = tenantID.String()
	gucCtx := pgcommon.WithGUCSet(ctx, g)
	return s.memberships.SetStatus(gucCtx, tenantID, userID, status, expectedVersion)
}

// DeleteMember is I-5: full cascade on Keycloak USER_DELETE. Soft-deletes
// tenant_memberships + cascades tenant_roles, dept_memberships, delegations,
// tender_acl_entries. Sets ownerless_since if we just removed the last
// active tenant_owner (TM-12/T-13). Emits Revoked + DelegationEnded
// (delegate_removed, DEL-7) atomically.
func (s *ProvisioningService) DeleteMember(ctx context.Context, tenantID, userID uuid.UUID) error {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = "iam-system"
	g.TenantID = tenantID.String()
	gucCtx := pgcommon.WithGUCSet(ctx, g)

	return s.txRunner.RunInTx(gucCtx, func(txCtx context.Context) error {
		// Look up membership (need expected version for soft delete).
		mem, err := s.memberships.FindByUserID(txCtx, tenantID, userID)
		if err != nil {
			return err
		}
		// Check whether this user is the last active tenant_owner BEFORE
		// cascading revokes.
		wasOwner := false
		trs, err := s.roles.ListByUser(txCtx, tenantID, userID)
		if err != nil {
			return err
		}
		for _, tr := range trs {
			if tr.RoleCode == domain.RoleTenantOwner {
				wasOwner = true
				break
			}
		}

		pub, _ := port.EventPublisherFromContext(txCtx)

		// Cascade 1: revoke all elevated roles.
		revokedRoles, err := s.roles.SoftDeleteAllForUser(txCtx, tenantID, userID)
		if err != nil {
			return err
		}
		for _, r := range revokedRoles {
			if pub != nil {
				_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
					Type: domain.EventTenantRoleRevoked, TenantID: tenantID,
					Subject: userID.String(), Actor: "iam-system",
					Data: domain.TenantRoleRevokedPayload{
						UserID: userID, TenantID: tenantID, RoleCode: r.RoleCode, ActorID: uuid.Nil,
					},
				})
			}
		}

		// Cascade 2: soft-delete dept memberships.
		revokedDepts, err := s.deptMems.SoftDeleteAllForUser(txCtx, tenantID, userID)
		if err != nil {
			return err
		}
		for _, d := range revokedDepts {
			if pub != nil {
				_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
					Type: domain.EventDepartmentMembershipRevoked, TenantID: tenantID,
					Subject: userID.String(), Actor: "iam-system",
					Data: domain.DepartmentMembershipRevokedPayload{
						UserID: userID, TenantID: tenantID, DepartmentID: d.DepartmentID,
					},
				})
			}
		}

		// Cascade 3: end active delegations (both directions).
		endedDelegations, err := s.delegations.SoftDeleteForUser(txCtx, tenantID, userID)
		if err != nil {
			return err
		}
		for _, d := range endedDelegations {
			if pub != nil {
				_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
					Type: domain.EventDelegationEnded, TenantID: tenantID,
					Subject: d.ID.String(), Actor: "iam-system",
					Data: domain.DelegationEndedPayload{
						DelegationID: d.ID, TenantID: tenantID,
						DelegatorID: d.DelegatorID, DelegateID: d.DelegateID,
						Scope: d.Scope, ScopeID: d.ScopeID,
						EndedReason: domain.EndReasonDelegateRemoved,
					},
				})
			}
		}

		// Cascade 4: soft-delete ACL grants for the user.
		if _, err := s.acls.SoftDeleteForUser(txCtx, tenantID, userID); err != nil {
			return err
		}

		// Membership itself.
		if err := s.memberships.SoftDelete(txCtx, tenantID, userID, mem.RecordVersion); err != nil {
			return err
		}

		// TM-12: if we just removed the last active owner, set
		// ownerless_since. Uses TxFromContext to hit the running tx.
		if wasOwner {
			ownerRemaining, err := s.roles.CountActiveOwners(txCtx, tenantID)
			if err != nil {
				return err
			}
			if ownerRemaining == 0 {
				if tx, ok := pgadapterTxFromContext(txCtx); ok {
					if _, err := tx.Exec(txCtx, `UPDATE tenants SET ownerless_since = now() WHERE id = $1 AND ownerless_since IS NULL`, tenantID); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}
