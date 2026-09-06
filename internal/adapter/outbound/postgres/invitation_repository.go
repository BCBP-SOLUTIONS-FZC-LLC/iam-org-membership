package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type InvitationRepository struct {
	pool *pgcommon.Pool
}

var _ port.InvitationRepository = (*InvitationRepository)(nil)

func NewInvitationRepository(pool *pgcommon.Pool) *InvitationRepository {
	return &InvitationRepository{pool: pool}
}

const inviteCols = `id, tenant_id, email, full_name, initial_tenant_roles, initial_dept_mappings,
	invited_by, keycloak_user_id, status, expires_at, accepted_at, kc_cleanup_pending,
	record_version, created_at, updated_at`

func scanInvitation(row pgx.Row) (*domain.PendingInvitation, error) {
	var inv domain.PendingInvitation
	var rolesArr []string
	var deptMappingsJSON []byte
	var status string
	if err := row.Scan(&inv.ID, &inv.TenantID, &inv.Email, &inv.FullName,
		&rolesArr, &deptMappingsJSON,
		&inv.InvitedBy, &inv.KeycloakUserID, &status, &inv.ExpiresAt, &inv.AcceptedAt,
		&inv.KCCleanupPending, &inv.RecordVersion, &inv.CreatedAt, &inv.UpdatedAt); err != nil {
		return nil, err
	}
	inv.Status = domain.InvitationStatus(status)
	inv.InitialTenantRoles = make([]domain.TenantRoleCode, len(rolesArr))
	for i, r := range rolesArr {
		inv.InitialTenantRoles[i] = domain.TenantRoleCode(r)
	}
	if len(deptMappingsJSON) > 0 && string(deptMappingsJSON) != "null" {
		if err := json.Unmarshal(deptMappingsJSON, &inv.InitialDeptMappings); err != nil {
			return nil, err
		}
	}
	if inv.InitialDeptMappings == nil {
		inv.InitialDeptMappings = []domain.InvitationDeptMapping{}
	}
	return &inv, nil
}

func (r *InvitationRepository) List(ctx context.Context, tenantID uuid.UUID) ([]domain.PendingInvitation, error) {
	var out []domain.PendingInvitation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+inviteCols+` FROM pending_invitations WHERE tenant_id = $1 AND status = 'pending' ORDER BY created_at DESC`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			inv, err := scanInvitation(rows)
			if err != nil {
				return err
			}
			out = append(out, *inv)
		}
		return rows.Err()
	})
	return out, err
}

func (r *InvitationRepository) LockByID(ctx context.Context, id uuid.UUID) (*domain.PendingInvitation, error) {
	var out *domain.PendingInvitation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		inv, err := scanInvitation(tx.QueryRow(ctx, `SELECT `+inviteCols+` FROM pending_invitations WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		out = inv
		return nil
	})
	return out, err
}

func (r *InvitationRepository) FindByID(ctx context.Context, tenantID, id uuid.UUID) (*domain.PendingInvitation, error) {
	var out *domain.PendingInvitation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+inviteCols+` FROM pending_invitations WHERE tenant_id = $1 AND id = $2`, tenantID, id)
		inv, err := scanInvitation(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.ErrInvitationNotFound, "invitation not found")
			}
			return err
		}
		out = inv
		return nil
	})
	return out, err
}

func (r *InvitationRepository) FindPendingByEmail(ctx context.Context, tenantID uuid.UUID, email string) (*domain.PendingInvitation, error) {
	var out *domain.PendingInvitation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		// LOWER on both sides makes lookup case-insensitive, so a REGISTER
		// webhook whose email casing differs from the invite still resolves
		// the pending row (PI-4 acceptance vs plain-add branch discriminator).
		row := tx.QueryRow(ctx, `SELECT `+inviteCols+` FROM pending_invitations WHERE tenant_id = $1 AND LOWER(email) = LOWER($2) AND status = 'pending'`, tenantID, email)
		inv, err := scanInvitation(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // no active invitation is OK — caller checks
			}
			return err
		}
		out = inv
		return nil
	})
	return out, err
}

func (r *InvitationRepository) FindPendingByKeycloakUser(ctx context.Context, tenantID, keycloakUserID uuid.UUID) (*domain.PendingInvitation, error) {
	var out *domain.PendingInvitation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+inviteCols+` FROM pending_invitations WHERE tenant_id = $1 AND keycloak_user_id = $2 AND status = 'pending'`, tenantID, keycloakUserID)
		inv, err := scanInvitation(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		out = inv
		return nil
	})
	return out, err
}

func (r *InvitationRepository) Insert(ctx context.Context, inv *domain.PendingInvitation) (*domain.PendingInvitation, error) {
	if inv.ID == uuid.Nil {
		inv.ID = uuid.New()
	}
	if inv.Status == "" {
		inv.Status = domain.InvitePending
	}
	rolesArr := make([]string, len(inv.InitialTenantRoles))
	for i, r := range inv.InitialTenantRoles {
		rolesArr[i] = string(r)
	}
	deptMappingsJSON, err := json.Marshal(inv.InitialDeptMappings)
	if err != nil {
		return nil, err
	}
	if len(inv.InitialDeptMappings) == 0 {
		deptMappingsJSON = []byte(`[]`)
	}

	var out *domain.PendingInvitation
	err = withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO pending_invitations (id, tenant_id, email, full_name,
				initial_tenant_roles, initial_dept_mappings,
				invited_by, keycloak_user_id, status, expires_at, kc_cleanup_pending)
			VALUES ($1, $2, $3, $4, $5::tenant_role[], $6::jsonb, $7, $8, $9, $10, false)
			RETURNING `+inviteCols,
			inv.ID, inv.TenantID, inv.Email, inv.FullName,
			rolesArr, string(deptMappingsJSON),
			inv.InvitedBy, inv.KeycloakUserID, string(inv.Status), inv.ExpiresAt)
		created, err := scanInvitation(row)
		if err != nil {
			// 23505 = unique_violation: concurrent invite for same email → invitation_already_exists (409)
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return domain.NewError(domain.ErrInvitationAlreadyExists,
					"invitation already exists for this email")
			}
			return err
		}
		out = created
		return nil
	})
	return out, err
}

func (r *InvitationRepository) SetKeycloakUserID(ctx context.Context, tenantID, id uuid.UUID, keycloakUserID uuid.UUID, expectedVersion int64) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE pending_invitations SET keycloak_user_id = $3
			WHERE tenant_id = $1 AND id = $2 AND record_version = $4`,
			tenantID, id, keycloakUserID, expectedVersion)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var v int64
			probe := tx.QueryRow(ctx, `SELECT record_version FROM pending_invitations WHERE tenant_id = $1 AND id = $2`, tenantID, id)
			if perr := probe.Scan(&v); perr != nil {
				if errors.Is(perr, pgx.ErrNoRows) {
					return domain.NewError(domain.ErrInvitationNotFound, "invitation not found")
				}
				return perr
			}
			return domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict").
				WithDetails(map[string]any{"record_version": v})
		}
		return nil
	})
}

func (r *InvitationRepository) SetStatus(ctx context.Context, tenantID, id uuid.UUID, status domain.InvitationStatus, expectedVersion int64) (*domain.PendingInvitation, error) {
	var out *domain.PendingInvitation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		acceptedAtClause := ""
		if status == domain.InviteAccepted {
			acceptedAtClause = ", accepted_at = now()"
		}
		row := tx.QueryRow(ctx, `
			UPDATE pending_invitations SET status = $3`+acceptedAtClause+`
			WHERE tenant_id = $1 AND id = $2 AND record_version = $4 AND status = 'pending'
			RETURNING `+inviteCols,
			tenantID, id, string(status), expectedVersion)
		inv, err := scanInvitation(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				var v int64
				var s string
				probe := tx.QueryRow(ctx, `SELECT record_version, status FROM pending_invitations WHERE tenant_id = $1 AND id = $2`, tenantID, id)
				if perr := probe.Scan(&v, &s); perr != nil {
					if errors.Is(perr, pgx.ErrNoRows) {
						return domain.NewError(domain.ErrInvitationNotFound, "invitation not found")
					}
					return perr
				}
				// Terminal state (revoked/accepted/expired) — treat as not found per LLD P-31.
				if s != "pending" {
					return domain.NewError(domain.ErrInvitationNotFound, "invitation not found")
				}
				return domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict").
					WithDetails(map[string]any{"record_version": v})
			}
			return err
		}
		out = inv
		return nil
	})
	return out, err
}

func (r *InvitationRepository) SetKCCleanupPending(ctx context.Context, tenantID, id uuid.UUID, pending bool, expectedVersion int64) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE pending_invitations SET kc_cleanup_pending = $3
			WHERE tenant_id = $1 AND id = $2 AND record_version = $4`,
			tenantID, id, pending, expectedVersion)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var v int64
			probe := tx.QueryRow(ctx, `SELECT record_version FROM pending_invitations WHERE tenant_id = $1 AND id = $2`, tenantID, id)
			if perr := probe.Scan(&v); perr != nil {
				if errors.Is(perr, pgx.ErrNoRows) {
					return domain.NewError(domain.ErrInvitationNotFound, "invitation not found")
				}
				return perr
			}
			return domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict").
				WithDetails(map[string]any{"record_version": v})
		}
		return nil
	})
}

func (r *InvitationRepository) CountPending(ctx context.Context, tenantID uuid.UUID) (int, error) {
	var n int
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM pending_invitations WHERE tenant_id = $1 AND status = 'pending' AND expires_at > now()`, tenantID).Scan(&n)
	})
	return n, err
}

func (r *InvitationRepository) ListExpiring(ctx context.Context, before time.Time, limit int) ([]domain.PendingInvitation, error) {
	var out []domain.PendingInvitation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+inviteCols+` FROM pending_invitations WHERE status = 'pending' AND expires_at < $1 LIMIT `+itoa(limit), before)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			inv, err := scanInvitation(rows)
			if err != nil {
				return err
			}
			out = append(out, *inv)
		}
		return rows.Err()
	})
	return out, err
}

func (r *InvitationRepository) ListPendingKCCleanup(ctx context.Context, limit int) ([]domain.PendingInvitation, error) {
	var out []domain.PendingInvitation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+inviteCols+` FROM pending_invitations WHERE kc_cleanup_pending = true LIMIT `+itoa(limit))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			inv, err := scanInvitation(rows)
			if err != nil {
				return err
			}
			out = append(out, *inv)
		}
		return rows.Err()
	})
	return out, err
}

// MostRecentCreatedAt returns the created_at of the most recent invitation
// for (tenant_id, email) regardless of status — PI-11 cooldown check.
func (r *InvitationRepository) MostRecentCreatedAt(ctx context.Context, tenantID uuid.UUID, email string) (time.Time, error) {
	var t time.Time
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT created_at FROM pending_invitations
			 WHERE tenant_id = $1 AND email = $2
			 ORDER BY created_at DESC LIMIT 1`,
			tenantID, email)
		return row.Scan(&t)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	return t, err
}

// CountCreatedInWindow returns the count of invitations created for the
// tenant since `since` — PI-12 per-tenant hourly rate check.
func (r *InvitationRepository) CountCreatedInWindow(ctx context.Context, tenantID uuid.UUID, since time.Time) (int, error) {
	var n int
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM pending_invitations
			 WHERE tenant_id = $1 AND created_at >= $2`,
			tenantID, since).Scan(&n)
	})
	return n, err
}

func (r *InvitationRepository) ExpireOverdue(ctx context.Context, limit int) (int, error) {
	var n int
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pending_invitations
			SET status = 'expired', kc_cleanup_pending = true
			WHERE id IN (
				SELECT id FROM pending_invitations
				WHERE status = 'pending' AND expires_at < now()
				ORDER BY expires_at
				LIMIT $1
			)`, limit)
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		return nil
	})
	return n, err
}

func (r *InvitationRepository) ClearKCCleanupPendingByID(ctx context.Context, id uuid.UUID) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE pending_invitations SET kc_cleanup_pending = false WHERE id = $1`, id)
		return err
	})
}
