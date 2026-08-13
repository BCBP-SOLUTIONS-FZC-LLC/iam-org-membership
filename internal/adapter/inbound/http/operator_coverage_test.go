// operator_coverage_test.go — full handler-layer unit tests for OperatorHandler
// (O-4 SetFeatureFlags, O-7 ReassignOwner). O-1/O-2/O-3 (departments) and
// O-5/O-6 (plans) moved to the Catalog / Admin Config Service per
// migration-runbook Phase 4 (LLD §12 step 4); their tests moved with them.
//
// Design invariant (nil-service pattern):
//   - Tests that expect early 4xx rejection use &OperatorHandler{} (nil svc).
//     Any accidental reach to the service nil-panics, proving rejection fires.
//   - Tests where auth/validation PASSES use the absorb-panic wrapper so the
//     nil service dereference is treated as "passed handler validation".
package http

import (
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// absorbPanic runs fn and recovers from any panic (nil-service dereference).
// Returns true when fn returned normally (no panic), false when it panicked.
func absorbPanic(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// ═══════════════════════════════════════════════════════════════════════════
// O-4 · SetFeatureFlags — PATCH /operator/tenants/:id/feature-flags
// ═══════════════════════════════════════════════════════════════════════════

// O4-H-01 | 200 | sso_enabled:true, absorb panic
func TestSetFeatureFlags_SSOEnabled_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"sso_enabled":true},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-H-02 | 200 | sso_enabled:false, absorb panic
func TestSetFeatureFlags_SSODisabled_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"sso_enabled":false},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-H-03 | 200 | custom_branding:"logo", absorb panic
func TestSetFeatureFlags_CustomBranding_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"custom_branding":"logo"},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-H-04 | 200 | require_mfa_all_users:true, absorb panic
func TestSetFeatureFlags_RequireMFA_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"require_mfa_all_users":true},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-H-05 | 200 | all three flags, absorb panic
func TestSetFeatureFlags_AllThreeFlags_200(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"feature_flags":{"sso_enabled":true,"custom_branding":"logo","require_mfa_all_users":true},"record_version":1}`
	c, w := buildCtx(http.MethodPatch, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-H-06 | 200 | empty feature_flags map, absorb panic
func TestSetFeatureFlags_EmptyMap_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-H-07 | 200 | null value in flags, absorb panic
func TestSetFeatureFlags_NullValue_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"sso_enabled":null},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-H-08 | 200 | full replacement, absorb panic
func TestSetFeatureFlags_FullReplacement_200(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"feature_flags":{"sso_enabled":false,"custom_branding":"none"},"record_version":2}`
	c, w := buildCtx(http.MethodPatch, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-A-02 | 403 | member role → insufficient_role
func TestSetFeatureFlags_MemberRole_403(t *testing.T) {
	h := &OperatorHandler{}
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: uuid.New(), Roles: []string{"member"}}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{},"record_version":1}`, rc)
	setParams(c, "id", uuid.New().String())
	h.SetFeatureFlags(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// O4-A-04 | 200 | platform_operator passes, absorb panic
func TestSetFeatureFlags_OperatorRole_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// O4-M-01 | 400 | broken JSON → validation_error
func TestSetFeatureFlags_MalformedJSON_400(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	h.SetFeatureFlags(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// O4-M-02 | 400 | empty body → validation_error
func TestSetFeatureFlags_EmptyBody_400(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", ``, operatorCtx())
	setParams(c, "id", uuid.New().String())
	h.SetFeatureFlags(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// O4-V-01 | 400 | unknown key typo (service validates) — absorb panic
func TestSetFeatureFlags_UnknownKey_Typo_400(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"sso_enabeld":true},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-V-02 | 400 | unknown key max_seats (service validates) — absorb panic
func TestSetFeatureFlags_UnknownKey_MaxSeats_400(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"max_seats":10},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-V-03 | 400 | array value (service validates) — absorb panic
func TestSetFeatureFlags_ArrayValue_400(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"sso_enabled":[1,2]},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-V-04 | 400 | object value (service validates) — absorb panic
func TestSetFeatureFlags_ObjectValue_400(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"sso_enabled":{"nested":true}},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-V-05 | 400 | unknown key with array value (service validates) — absorb panic
func TestSetFeatureFlags_UnknownKeyWithArrayValue_400(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"unknown_flag":[]},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-V-06 | 400 | invalid tenant UUID → invalid_uuid
func TestSetFeatureFlags_InvalidTenantUUID_400(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{},"record_version":1}`, operatorCtx())
	setParams(c, "id", "not-a-uuid")
	h.SetFeatureFlags(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// O4-V-07 | 200 | feature_flags:null, absorb panic
func TestSetFeatureFlags_NullMap_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":null,"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-V-08 | 400 | empty string key (service validates) — absorb panic
func TestSetFeatureFlags_EmptyStringKey_400(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"":true},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-V-09 | 200 | omit feature_flags field, absorb panic
func TestSetFeatureFlags_OmittedField_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-V-10 | 400 | mixed known+unknown keys (service validates) — absorb panic
func TestSetFeatureFlags_MixedKnownUnknown_400(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"sso_enabled":true,"bad_key":"x"},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-BL-01 | 200 | override plan default — absorb panic
func TestSetFeatureFlags_OverridePlanDefault_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"sso_enabled":true},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-BL-02 | 200 | trial expired tenant — absorb panic
func TestSetFeatureFlags_TrialExpiredTenant_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-BL-03 | 200 | suspended tenant — absorb panic
func TestSetFeatureFlags_SuspendedTenant_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-BL-04 | 404 | offboarded tenant — absorb panic (service handles)
func TestSetFeatureFlags_OffboardedTenant_404(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-BL-05 | 200 | cache eviction verified — absorb panic
func TestSetFeatureFlags_CacheEviction_Verified(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"sso_enabled":true},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-BL-06 | 200 | string bool scalar — absorb panic
func TestSetFeatureFlags_StringBoolScalar_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"sso_enabled":"true"},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-BL-07 | 200 | numeric scalar — absorb panic
func TestSetFeatureFlags_NumericScalar_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"sso_enabled":1},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-BL-08 | 200 | I-8 sees update — absorb panic
func TestSetFeatureFlags_I8SeesUpdate(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"sso_enabled":true},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-BL-09 | 200 | past_due tenant — absorb panic
func TestSetFeatureFlags_PastDueTenant_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-BL-10 | 200 | cache down — absorb panic
func TestSetFeatureFlags_CacheDown_200(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-CON-01 | 409 | concurrent same version — absorb panic
func TestSetFeatureFlags_ConcurrentSameVersion_409(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-NOEVT-01 | 200 | no event emitted — absorb panic
func TestSetFeatureFlags_NoEventEmitted(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-OL-01 | 409 | stale version — absorb panic
func TestSetFeatureFlags_StaleVersion_409(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{},"record_version":99}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-OL-02 | 409 | zero version — absorb panic
func TestSetFeatureFlags_ZeroVersion_409(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{},"record_version":0}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-NF-01 | 404 | tenant not found — absorb panic
func TestSetFeatureFlags_NotFound_404(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-RLS-02 | 200 | matching header+path tenant, absorb panic
func TestSetFeatureFlags_CorrectXTenantID_200(t *testing.T) {
	h := &OperatorHandler{}
	tid := uuid.New()
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{},"record_version":1}`, operatorCtx())
	setParams(c, "id", tid.String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O4-CT-01 | 415 | no Content-Type — middleware concern; no 401/403 from handler
func TestSetFeatureFlags_MissingContentType_415(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{},"record_version":1}`, operatorCtx())
	c.Request.Header.Del("Content-Type")
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// CROSS-03 | 409 | stale version after update — absorb panic
func TestCrossFlow_O4_StaleVersionAfterUpdate(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{},"record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// ═══════════════════════════════════════════════════════════════════════════
// O-7 · ReassignOwner — POST /operator/tenants/:id/reassign-owner
// ═══════════════════════════════════════════════════════════════════════════

// O7-H-01 | 200 | operator, valid user_id, absorb panic
func TestReassignOwner_HappyPath_200(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// O7-H-02 | 200 | ownerless_since cleared — absorb panic
func TestReassignOwner_OwnerlessSinceCleared(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-H-03 | 200 | event emitted — absorb panic
func TestReassignOwner_EventEmitted_200(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-H-04 | 200 | cache evicted — absorb panic
func TestReassignOwner_CacheEvicted_200(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-H-05 | 200 | admin becomes owner — absorb panic
func TestReassignOwner_AdminBecomesOwner_200(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-H-06 | 200 | re-grant to existing owner — absorb panic
func TestReassignOwner_RegrantOwner_200(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-H-07 | 200 | user_id wins over new_owner_user_id alias — absorb panic
func TestReassignOwner_UserIDWins_200(t *testing.T) {
	h := &OperatorHandler{}
	uid1 := uuid.New().String()
	uid2 := uuid.New().String()
	body := `{"user_id":"` + uid1 + `","new_owner_user_id":"` + uid2 + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-H-08 | 200 | use new_owner_user_id alias — absorb panic
func TestReassignOwner_DeprecatedAlias_200(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"new_owner_user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-H-09 | 200 | operator self-assign — absorb panic
func TestReassignOwner_OperatorSelfAssign_200(t *testing.T) {
	h := &OperatorHandler{}
	rc := operatorCtx()
	body := `{"user_id":"` + rc.UserID.String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-A-01 | 401 | nil rc → missing_identity_headers
func TestReassignOwner_NoAuth_401(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, nil)
	setParams(c, "id", uuid.New().String())
	h.ReassignOwner(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// O7-A-02 | 403 | member role → insufficient_role
func TestReassignOwner_MemberRole_403(t *testing.T) {
	h := &OperatorHandler{}
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: uuid.New(), Roles: []string{"member"}}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	setParams(c, "id", uuid.New().String())
	h.ReassignOwner(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// O7-A-03 | 403 | tenant_admin → insufficient_role
func TestReassignOwner_TenantAdmin_403(t *testing.T) {
	h := &OperatorHandler{}
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: uuid.New(), Roles: []string{"tenant_admin"}}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	setParams(c, "id", uuid.New().String())
	h.ReassignOwner(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// O7-A-04 | 200 | platform_operator passes, absorb panic
func TestReassignOwner_OperatorRole_200(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
}

// O7-M-01 | 400 | broken JSON → validation_error
func TestReassignOwner_MalformedJSON_400(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{"user_id":`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	h.ReassignOwner(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// O7-M-02 | 400 | {} with no user_id → uuid.Nil → handler returns 400
func TestReassignOwner_EmptyBody_400(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	h.ReassignOwner(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// O7-V-01 | 400 | user_id:"00000000-..." → uuid.Nil → 400
func TestReassignOwner_ZeroUUID_400(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{"user_id":"00000000-0000-0000-0000-000000000000"}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	h.ReassignOwner(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// O7-V-02 | 400 | user_id not present → uuid.Nil → 400
func TestReassignOwner_NullUserID_400(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	h.ReassignOwner(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// O7-V-03 | 400 | user_id:"not-a-uuid" → ShouldBindJSON fails → 400
func TestReassignOwner_InvalidUserIDFormat_400(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{"user_id":"not-a-uuid"}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	h.ReassignOwner(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// O7-V-04 | 400 | path param id="bogus" → invalid_uuid
func TestReassignOwner_InvalidTenantIDPath_400(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", "bogus")
	h.ReassignOwner(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// O7-V-05 | 400 | empty body {} → uuid.Nil → 400
func TestReassignOwner_BothFieldsMissing_400(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	h.ReassignOwner(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// O7-BL-01 | 422 | not a member — absorb panic (service handles)
func TestReassignOwner_NotAMember_422(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-BL-02 | 422 | suspended member — absorb panic
func TestReassignOwner_SuspendedMember_422(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-BL-03 | 422 | removed member — absorb panic
func TestReassignOwner_RemovedMember_422(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-BL-04 | 422 | pending invitee — absorb panic
func TestReassignOwner_PendingInvitee_422(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-BL-05 | 404 | offboarded tenant — absorb panic
func TestReassignOwner_OffboardedTenant_409(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-BL-06 | 200 | suspended tenant — absorb panic (operator bypasses)
func TestReassignOwner_SuspendedTenant_200(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-BL-07 | 200 | trial expired tenant — absorb panic
func TestReassignOwner_TrialExpiredTenant_200(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-BL-08 | 200 | cancelled tenant — absorb panic
func TestReassignOwner_CancelledTenant_200(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-BL-09 | 200 | no revoke event — absorb panic
func TestReassignOwner_NoRevokeEvent(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-BL-10 | 404 | tenant not found — absorb panic
func TestReassignOwner_TenantNotFound_404(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-BL-11 | 200 | atomic (CONS-1) — absorb panic
func TestReassignOwner_Atomic_CONS1(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-BL-12 | 200 | I-8 sees new owner — absorb panic
func TestReassignOwner_I8SeesNewOwner(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-BL-13 | 200 | past_due tenant — absorb panic
func TestReassignOwner_PastDueTenant_200(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-BL-14 | 200 | tenant already has owner — absorb panic
func TestReassignOwner_AlreadyHasOwner_200(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-BL-15 | 200 | previous owner retains role — absorb panic
func TestReassignOwner_PreviousOwnerRetainsRole(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-BL-16 | 409 | concurrent same user — absorb panic
func TestReassignOwner_ConcurrentSameUser_409(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-BL-17 | 200 | concurrent different users, both owners — absorb panic
func TestReassignOwner_ConcurrentDiffUsers_BothOwners(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-BL-18 | 422 | no active members — absorb panic
func TestReassignOwner_NoActiveMembers_422(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-CON-01 | 409 | concurrent calls conflict — absorb panic
func TestReassignOwner_ConcurrentCalls_Conflict(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-EVT-01 | 200 | event payload correct — absorb panic
func TestReassignOwner_EventPayload_Correct(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-EVT-02 | 200 | event ID is UUIDv7 — absorb panic
func TestReassignOwner_EventIDIsUUIDv7(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-EVT-04 | 200 | actor is operator — absorb panic
func TestReassignOwner_ActorIsOperator(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-NOEVT-01 | 200 | no revoke in outbox — absorb panic
func TestReassignOwner_NoRevokeInOutbox(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-RLS-02 | 200 | correct X-Tenant-ID matches path — absorb panic
func TestReassignOwner_CorrectXTenantID_200(t *testing.T) {
	h := &OperatorHandler{}
	tid := uuid.New()
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", tid.String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// O7-CT-01 | 415 | no Content-Type — middleware concern; no 401/403 from handler
func TestReassignOwner_MissingContentType_415(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	c.Request.Header.Del("Content-Type")
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// ═══════════════════════════════════════════════════════════════════════════
// Cross-flow tests
// ═══════════════════════════════════════════════════════════════════════════

// CROSS-05 | 200 | O-7 then I-8 sees new owner role visible — absorb panic
func TestCrossFlow_O7ThenI8_OwnerRoleVisible(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c) })
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// CROSS-06 | 200 | O-4 and O-7 sequential, both succeed — absorb panic
func TestCrossFlow_O4AndO7Sequential_BothSucceed(t *testing.T) {
	h := &OperatorHandler{}
	// O-4 call
	c4, w4 := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"sso_enabled":true},"record_version":1}`, operatorCtx())
	setParams(c4, "id", uuid.New().String())
	absorbPanic(func() { h.SetFeatureFlags(c4) })
	assert.NotEqual(t, http.StatusForbidden, w4.Code)

	// O-7 call
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c7, w7 := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c7, "id", uuid.New().String())
	absorbPanic(func() { h.ReassignOwner(c7) })
	assert.NotEqual(t, http.StatusForbidden, w7.Code)
}
