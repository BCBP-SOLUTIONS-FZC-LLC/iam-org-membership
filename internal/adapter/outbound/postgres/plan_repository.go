package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
)

type PlanRepository struct {
	pool *pgcommon.Pool
}

var _ port.PlanRepository = (*PlanRepository)(nil)

func NewPlanRepository(pool *pgcommon.Pool) *PlanRepository {
	return &PlanRepository{pool: pool}
}

const planCols = `code, display_name, workflow_template_limit, tender_limit, trial_duration_days, sso_enabled, custom_branding, feature_set, record_version, created_at, updated_at`

func scanPlan(row pgx.Row) (*domain.Plan, error) {
	var p domain.Plan
	var code, branding string
	var featureSetJSON []byte
	if err := row.Scan(&code, &p.DisplayName, &p.WorkflowTemplateLimit, &p.TenderLimit,
		&p.TrialDurationDays, &p.SSOEnabled, &branding, &featureSetJSON,
		&p.RecordVersion, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.Code = domain.TenantPlan(code)
	p.CustomBranding = domain.BrandingLevel(branding)
	if len(featureSetJSON) > 0 && string(featureSetJSON) != "null" {
		if err := json.Unmarshal(featureSetJSON, &p.FeatureSet); err != nil {
			return nil, err
		}
	}
	if p.FeatureSet == nil {
		p.FeatureSet = map[string]any{}
	}
	return &p, nil
}

func (r *PlanRepository) List(ctx context.Context) ([]domain.Plan, error) {
	var out []domain.Plan
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+planCols+` FROM plans ORDER BY code`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPlan(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		return rows.Err()
	})
	return out, err
}

func (r *PlanRepository) FindByCode(ctx context.Context, code domain.TenantPlan) (*domain.Plan, error) {
	var out *domain.Plan
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+planCols+` FROM plans WHERE code = $1`, string(code))
		p, err := scanPlan(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.ErrValidation, "plan not found").
					WithDetails(map[string]any{"code": "invalid_plan"})
			}
			return err
		}
		out = p
		return nil
	})
	return out, err
}

// Update applies PlanPatch with optimistic locking. The double-pointer on
// limits distinguishes "not set" from "set to nil (unlimited)".
func (r *PlanRepository) Update(ctx context.Context, code domain.TenantPlan, patch *domain.PlanPatch) (*domain.Plan, error) {
	if patch == nil {
		return nil, domain.NewError(domain.ErrValidation, "patch is required")
	}
	sets := []string{}
	args := []any{string(code), patch.RecordVersion}
	next := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if patch.DisplayName != nil {
		sets = append(sets, "display_name = "+next(*patch.DisplayName))
	}
	if patch.WorkflowTemplateLimit != nil {
		sets = append(sets, "workflow_template_limit = "+next(*patch.WorkflowTemplateLimit))
	}
	if patch.TenderLimit != nil {
		sets = append(sets, "tender_limit = "+next(*patch.TenderLimit))
	}
	if patch.TrialDurationDays != nil {
		sets = append(sets, "trial_duration_days = "+next(*patch.TrialDurationDays))
	}
	if patch.SSOEnabled != nil {
		sets = append(sets, "sso_enabled = "+next(*patch.SSOEnabled))
	}
	if patch.CustomBranding != nil {
		sets = append(sets, "custom_branding = "+next(string(*patch.CustomBranding)))
	}
	if patch.FeatureSet != nil {
		fsJSON, err := json.Marshal(patch.FeatureSet)
		if err != nil {
			return nil, err
		}
		sets = append(sets, "feature_set = "+next(string(fsJSON))+"::jsonb")
	}
	if len(sets) == 0 {
		return r.FindByCode(ctx, code)
	}
	sql := `UPDATE plans SET ` + strings.Join(sets, ", ") +
		` WHERE code = $1 AND record_version = $2 RETURNING ` + planCols

	var out *domain.Plan
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, sql, args...)
		p, err := scanPlan(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				var v int64
				probe := tx.QueryRow(ctx, `SELECT record_version FROM plans WHERE code = $1`, string(code))
				if perr := probe.Scan(&v); perr != nil {
					if errors.Is(perr, pgx.ErrNoRows) {
						return domain.NewError(domain.ErrValidation, "plan not found").
							WithDetails(map[string]any{"code": "invalid_plan"})
					}
					return perr
				}
				return domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict").
					WithDetails(map[string]any{"record_version": v})
			}
			return err
		}
		out = p
		return nil
	})
	return out, err
}
