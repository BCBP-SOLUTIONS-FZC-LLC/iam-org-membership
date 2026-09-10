//go:build integration

// Phase 4 — service-layer integration tests. Each test wires the full
// service graph (via buildTestFixtures) against a fresh testcontainers
// Postgres, invokes a real service method, then asserts on observable
// state (outbox_events rows, DB table state, prometheus counters).
//
// Coverage focus: the audit findings that couldn't be tested at the pure
// repo layer (Phase 2 postgres tests):
//
//	G1  — Trial signup activates EXACTLY 5 named departments.
//	B5  — JIT SAML emits Granted / LevelChanged / no-event based on
//	      prior dept-membership state (LLD rev 0.69, TRG-3 discipline).
//	B15 — DeptMembershipService classifies concurrent Assign calls
//	      correctly (Granted vs LevelChanged race-safe).
//	B1  — O-7 ReassignOwner emits TenantRoleGranted via TxRunner.
//	B13 — TM-12 last-owner escalation increments the prometheus counter.
package postgres_test

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	pmodel "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pgxpoolPoolReal is the concrete pool type used by helpers below.
type pgxpoolPoolReal = dbseed.Pool

// ─────────────────────────────────────────────────────────────────────────
// G1: Trial signup activates exactly the 5 named system departments per
// LLD §8.1 — ENGINEERING, DESIGN, PROCUREMENT, FINANCE, LEGAL.
// ─────────────────────────────────────────────────────────────────────────

func TestG1_TrialSignup_ActivatesExactly5NamedDepartments(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	tenantID := uuid.New()
	ownerID := uuid.New()
	tctx := withSystemAndTenant(ctx, tenantID)

	_, _, err := fx.Provisioning.TrialSignup(tctx, service.TrialSignupInput{
		TenantID:      tenantID,
		Slug:          "acme-g1",
		Name:          "Acme G1",
		Plan:          domain.PlanStarter,
		OwnerUserID:   ownerID,
		DefaultLocale: "en-US",
	})
	require.NoError(t, err)

	// Resolve the exact department codes activated for this tenant. The
	// departments table was dropped (migration-runbook Phase 4 — LLD §12
	// step 4); tenant_departments.department_id now resolves against
	// fx.CatalogDepts instead of a local JOIN.
	rows, err := fx.rawPool.Query(ctx,
		`SELECT department_id FROM tenant_departments WHERE tenant_id = $1`, tenantID)
	require.NoError(t, err)
	defer rows.Close()

	var codes []string
	for rows.Next() {
		var deptID uuid.UUID
		require.NoError(t, rows.Scan(&deptID))
		d, derr := fx.CatalogDepts.DepartmentByID(ctx, deptID)
		require.NoError(t, derr)
		codes = append(codes, d.Code)
	}
	require.NoError(t, rows.Err())
	sort.Strings(codes)

	expected := []string{"DESIGN", "ENGINEERING", "FINANCE", "LEGAL", "PROCUREMENT"}
	assert.Equal(t, expected, codes,
		"G1: LLD §8.1 fixes trial to exactly these 5 codes — a 6th is_system dept must NOT auto-activate")
	assert.Len(t, codes, 5, "exactly 5 depts activated")
}

// TrialSignup also stamps the standard 3-event storm on iam.tenant.events +
// iam.membership.events. Verify the outbox rows match §7.3.
func TestG1_TrialSignup_EmitsExpectedEvents(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	tenantID := uuid.New()
	ownerID := uuid.New()
	tctx := withSystemAndTenant(ctx, tenantID)

	_, _, err := fx.Provisioning.TrialSignup(tctx, service.TrialSignupInput{
		TenantID:      tenantID,
		Slug:          "acme-g1e",
		Name:          "Acme G1E",
		Plan:          domain.PlanStarter,
		OwnerUserID:   ownerID,
		DefaultLocale: "en-US",
	})
	require.NoError(t, err)

	types := fx.outboxEventTypesForTenant(t, ctx, tenantID)
	assert.Contains(t, types, domain.EventTenantCreated, "TenantCreated emitted")
	assert.Contains(t, types, domain.EventTrialStarted, "TrialStarted emitted")
	assert.Contains(t, types, domain.EventTenantRoleGranted, "owner grant emitted")
}

// ─────────────────────────────────────────────────────────────────────────
// B5: JIT SAML emits the right event type based on prior dept-membership
// state — Granted, LevelChanged, or no-event (TRG-3 no-op).
// ─────────────────────────────────────────────────────────────────────────

func TestJIT_FirstTimeAssignment_EmitsGranted(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "acme-b5a")
	tctx := withSystemAndTenant(ctx, tenantID)

	// Establish an active tenant_membership + tenant_department, and stub
	// Group Mapping Service's GM-I1 resolution (ADR-0007 Wave 2 — I-10 no
	// longer reads group_dept_mappings/group_dept_role_mappings directly)
	// so JIT resolves to a real dept assignment.
	deptID := seedSystemDept(t, ctx, fx.CatalogDepts, "ENG_B5A", "Eng B5A")
	activateDept(t, ctx, fx.rawPool, tenantID, deptID)
	fx.GroupMappingClient.resolveFn = func(_ context.Context, _ uuid.UUID, _ []string) (*port.GroupResolution, error) {
		return &port.GroupResolution{
			DeptMappings:     []port.ResolvedDeptMapping{{KeycloakGroupName: "eng-team", DepartmentID: deptID}},
			DeptRoleMappings: []port.ResolvedDeptRoleMapping{{KeycloakGroupName: "eng-team", RoleCode: domain.DeptReviewer}},
		}, nil
	}

	_, err := fx.GroupMapping.AssignFromGroups(tctx, tenantID, ownerID, []string{"eng-team"})
	require.NoError(t, err)

	// First assignment → Granted, not LevelChanged, not empty.
	types := fx.outboxEventTypesForTenant(t, ctx, tenantID)
	assert.Contains(t, types, domain.EventDepartmentMembershipGranted)
	assert.NotContains(t, types, domain.EventDepartmentMembershipLevelChanged)
}

func TestJIT_SameLevelReplay_EmitsNothing(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "acme-b5b")
	tctx := withSystemAndTenant(ctx, tenantID)

	deptID := seedSystemDept(t, ctx, fx.CatalogDepts, "ENG_B5B", "Eng B5B")
	activateDept(t, ctx, fx.rawPool, tenantID, deptID)
	fx.GroupMappingClient.resolveFn = func(_ context.Context, _ uuid.UUID, _ []string) (*port.GroupResolution, error) {
		return &port.GroupResolution{
			DeptMappings:     []port.ResolvedDeptMapping{{KeycloakGroupName: "eng-team", DepartmentID: deptID}},
			DeptRoleMappings: []port.ResolvedDeptRoleMapping{{KeycloakGroupName: "eng-team", RoleCode: domain.DeptReviewer}},
		}, nil
	}

	// First JIT — establishes the row.
	_, err := fx.GroupMapping.AssignFromGroups(tctx, tenantID, ownerID, []string{"eng-team"})
	require.NoError(t, err)
	baselineCount := fx.countOutboxEvents(t, ctx, tenantID)

	// Second JIT with SAME groups/level → TRG-3 no-op, no new event.
	_, err = fx.GroupMapping.AssignFromGroups(tctx, tenantID, ownerID, []string{"eng-team"})
	require.NoError(t, err)
	assert.Equal(t, baselineCount, fx.countOutboxEvents(t, ctx, tenantID),
		"B5/TRG-3: identical JIT re-login must emit nothing")
}

func TestJIT_LevelChange_EmitsLevelChanged(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "acme-b5c")
	tctx := withSystemAndTenant(ctx, tenantID)

	deptID := seedSystemDept(t, ctx, fx.CatalogDepts, "ENG_B5C", "Eng B5C")
	activateDept(t, ctx, fx.rawPool, tenantID, deptID)
	// Map same group to two different levels via two resolveFn swaps.
	fx.GroupMappingClient.resolveFn = func(_ context.Context, _ uuid.UUID, _ []string) (*port.GroupResolution, error) {
		return &port.GroupResolution{
			DeptMappings:     []port.ResolvedDeptMapping{{KeycloakGroupName: "eng-team", DepartmentID: deptID}},
			DeptRoleMappings: []port.ResolvedDeptRoleMapping{{KeycloakGroupName: "eng-team", RoleCode: domain.DeptPreparator}},
		}, nil
	}

	_, err := fx.GroupMapping.AssignFromGroups(tctx, tenantID, ownerID, []string{"eng-team"})
	require.NoError(t, err)

	// Flip the role_code in the mapping to reviewer, then re-JIT.
	fx.GroupMappingClient.resolveFn = func(_ context.Context, _ uuid.UUID, _ []string) (*port.GroupResolution, error) {
		return &port.GroupResolution{
			DeptMappings:     []port.ResolvedDeptMapping{{KeycloakGroupName: "eng-team", DepartmentID: deptID}},
			DeptRoleMappings: []port.ResolvedDeptRoleMapping{{KeycloakGroupName: "eng-team", RoleCode: domain.DeptReviewer}},
		}, nil
	}
	_, err = fx.GroupMapping.AssignFromGroups(tctx, tenantID, ownerID, []string{"eng-team"})
	require.NoError(t, err)

	types := fx.outboxEventTypesForTenant(t, ctx, tenantID)
	// At least one LevelChanged in the outbox (in addition to the initial Granted).
	assert.Contains(t, types, domain.EventDepartmentMembershipLevelChanged,
		"B5: level change on JIT must emit LevelChanged, not another Granted")
}

// ─────────────────────────────────────────────────────────────────────────
// B15: DeptMembershipService classifies concurrent Assign calls correctly.
// The `previous` snapshot must be captured INSIDE the tx so a race can't
// misclassify Granted vs LevelChanged.
// ─────────────────────────────────────────────────────────────────────────

func TestConcurrentAssign_NoMisclassification(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, userID := seedTenantWithOwner(t, ctx, fx, "acme-b15")
	tctx := withSystemAndTenant(ctx, tenantID)

	deptID := seedSystemDept(t, ctx, fx.CatalogDepts, "ENG_B15", "Eng B15")
	activateDept(t, ctx, fx.rawPool, tenantID, deptID)

	// Two concurrent Assign calls for the SAME (tenant, user, dept) with
	// the SAME level. Both should resolve; at most ONE Granted event
	// should end up in the outbox (the other is a no-op per TRG-3).
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			time.Sleep(time.Duration(idx) * 3 * time.Millisecond)
			_, err := fx.DeptMembership.Assign(tctx, tenantID, userID, deptID, domain.DeptReviewer, userID)
			errs[idx] = err
		}(i)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	// Count DepartmentMembershipGranted events for this tenant.
	types := fx.outboxEventTypesForTenant(t, ctx, tenantID)
	granted := 0
	for _, et := range types {
		if et == domain.EventDepartmentMembershipGranted {
			granted++
		}
	}
	// Exactly one Granted (the winner); the racing goroutine sees existing
	// row at same level and emits nothing.
	assert.Equal(t, 1, granted,
		"B15: concurrent Assign for same key+level must emit exactly one Granted, not two")
}

// ─────────────────────────────────────────────────────────────────────────
// B1: O-7 ReassignOwner emits TenantRoleGranted via TxRunner.
// ─────────────────────────────────────────────────────────────────────────

func TestReassignOwner_EmitsTenantRoleGranted(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, currentOwner := seedTenantWithOwner(t, ctx, fx, "acme-b1")

	// Seed the new-owner candidate as an active member.
	newOwnerID := uuid.New()
	_, err := fx.rawPool.Exec(ctx, `
		INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		VALUES (gen_random_uuid(), $1, $2, 'active')`,
		tenantID, newOwnerID)
	require.NoError(t, err)

	tctx := withSystemAndTenant(ctx, tenantID)
	baseline := fx.countOutboxEvents(t, ctx, tenantID)

	_, err = fx.Operator.ReassignOwner(tctx, tenantID, newOwnerID, currentOwner)
	require.NoError(t, err)

	assert.Greater(t, fx.countOutboxEvents(t, ctx, tenantID), baseline,
		"B1: ReassignOwner must enqueue at least one event (TenantRoleGranted)")

	// Confirm the event type + subject.
	rows, err := fx.rawPool.Query(ctx, `
		SELECT event_type, payload::text FROM outbox_events
		WHERE tenant_id = $1
		ORDER BY created_at DESC LIMIT 5`, tenantID.String())
	require.NoError(t, err)
	defer rows.Close()
	var sawGranted bool
	for rows.Next() {
		var eventType, payload string
		require.NoError(t, rows.Scan(&eventType, &payload))
		if eventType == domain.EventTenantRoleGranted {
			sawGranted = true
			assert.Contains(t, payload, newOwnerID.String(),
				"payload must reference the new owner")
			assert.Contains(t, payload, string(domain.RoleTenantOwner),
				"payload must carry role_code=tenant_owner")
		}
	}
	assert.True(t, sawGranted, "B1: TenantRoleGranted must be in the outbox")
}

// ─────────────────────────────────────────────────────────────────────────
// B13: TM-12 escalation increments the prometheus counter when the last
// active tenant_owner is removed.
// ─────────────────────────────────────────────────────────────────────────

func TestTM12Escalation_IncrementsCounter(t *testing.T) {
	t.Parallel()
	// Register metrics once per test process. Register() is idempotent
	// (sync.Once) but we still gate so a parallel-package helper stays cheap.
	phase4TestMetricsInit()
	require.NotNil(t, metrics.TenantOwnerlessEscalated,
		"metrics must be initialised before this test")

	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "acme-b13")
	tctx := withSystemAndTenant(ctx, tenantID)

	before := counterValue(metrics.TenantOwnerlessEscalated.WithLabelValues("user_removed"))

	// Remove the sole owner — should flip ownerless_since AND increment counter.
	err := fx.Provisioning.DeleteMember(tctx, tenantID, ownerID)
	require.NoError(t, err)

	after := counterValue(metrics.TenantOwnerlessEscalated.WithLabelValues("user_removed"))
	assert.Equal(t, before+1, after,
		"B13: removing the sole owner must increment iam_org_membership_tenant_ownerless_escalated_total by 1")

	// Verify the tenant is now flagged ownerless.
	var since *time.Time
	err = fx.rawPool.QueryRow(ctx,
		`SELECT ownerless_since FROM tenants WHERE id = $1`, tenantID).Scan(&since)
	require.NoError(t, err)
	assert.NotNil(t, since, "tenants.ownerless_since must be stamped on TM-12 escalation")
}

// ─────────────────────────────────────────────────────────────────────────
// helpers used by phase 4 tests
// ─────────────────────────────────────────────────────────────────────────

// withSystemAndTenant builds a ctx with the app-role GUCSet (iam-system
// user, target tenant) so RLS WITH CHECK passes on inserts.
func withSystemAndTenant(ctx context.Context, tenantID uuid.UUID) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = "iam-system"
	g.TenantID = tenantID.String()
	return pgcommon.WithGUCSet(ctx, g)
}

// seedTenantWithOwner creates a tenant + one active tenant_membership +
// a tenant_owner role grant. Returns (tenantID, ownerUserID).
func seedTenantWithOwner(t testing.TB, ctx context.Context, fx *testFixtures, slug string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	tenantID := seedTenant(t, ctx, fx.rawPool, slug)
	ownerID := uuid.New()
	var memID uuid.UUID
	err := fx.rawPool.QueryRow(ctx, `
		INSERT INTO tenant_memberships (id, tenant_id, user_id, status)
		VALUES (gen_random_uuid(), $1, $2, 'active')
		RETURNING id`, tenantID, ownerID).Scan(&memID)
	require.NoError(t, err)
	_, err = fx.rawPool.Exec(ctx, `
		INSERT INTO tenant_roles (id, tenant_id, user_id, tenant_membership_id, role_code, granted_by)
		VALUES (gen_random_uuid(), $1, $2, $3, 'tenant_owner', $2)`,
		tenantID, ownerID, memID)
	require.NoError(t, err)
	return tenantID, ownerID
}

// seedSystemDept registers a system (is_system=true, is_active=true)
// department. The departments table was dropped (migration-runbook Phase
// 4 — LLD §12 step 4); catalog is nil for tests that construct repos
// directly without a fixture (department_id no longer needs to resolve
// against anything at the DB level — there's no FK left to satisfy).
func seedSystemDept(t *testing.T, ctx context.Context, catalog *fakeCatalogDepartments, code, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if catalog != nil {
		catalog.add(domain.Department{ID: id, Code: code, Name: name, IsSystem: true, IsActive: true})
	}
	return id
}

func activateDept(t *testing.T, ctx context.Context, rawPool *pgxpoolPool, tenantID, deptID uuid.UUID) {
	t.Helper()
	_, err := rawPool.Exec(ctx,
		`INSERT INTO tenant_departments (tenant_id, department_id, is_active) VALUES ($1, $2, true)`,
		tenantID, deptID)
	require.NoError(t, err)
}

// pgxpoolPool is an alias for *pgxpool.Pool so helper signatures stay readable
// without dragging the pgxpool import into every helper's declaration line.
type pgxpoolPool = pgxpoolPoolReal

// counterValue reads the current value of a prometheus counter.
func counterValue(counter interface {
	Write(*pmodel.Metric) error
}) float64 {
	m := &pmodel.Metric{}
	if err := counter.Write(m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// phase4TestMetricsInit registers the business metrics collector exactly
// once across the test binary, guarding against MustRegister-panic on
// re-run.
var phase4MetricsOnce sync.Once

func phase4TestMetricsInit() { phase4MetricsOnce.Do(func() { metrics.Register("test") }) }
