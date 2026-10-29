// CR-4 · CheckTrialEligibility handler tests: 200 eligible, 409
// TRIAL_ALREADY_USED, 400 missing or malformed email_hash, 403
// non-iam-system caller, 503 ledger DB outage, nil-ledger fail-open.
package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubLedger satisfies port.TrialSignupLedgerRepository for unit tests.
type stubLedger struct {
	exists  bool
	err     error
	checked []string
}

func (s *stubLedger) ExistsEmailHash(_ context.Context, h string) (bool, error) {
	s.checked = append(s.checked, h)
	return s.exists, s.err
}
func (s *stubLedger) ClaimEmailHash(context.Context, string, uuid.UUID) error { return nil }
func (s *stubLedger) PruneCreatedBefore(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

// validHash is a well-formed CR-4 hash (64 lower-case hex characters).
var validHash = strings.Repeat("ab", 32)

var _ port.TrialSignupLedgerRepository = (*stubLedger)(nil)

func newTrialEligibilityHandler(ledger port.TrialSignupLedgerRepository) *InternalHandler {
	svc := service.NewProvisioningService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).
		WithTrialLedger(ledger)
	return &InternalHandler{provisioning: svc}
}

func callTrialEligibility(h *InternalHandler, body string) *httptest.ResponseRecorder {
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	h.CheckTrialEligibility(c)
	return w
}

func decodeTrialEligibilityBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
	return out
}

// CR4-H-01: email not previously used → 200 eligible:true.
func TestCheckTrialEligibility_Eligible(t *testing.T) {
	h := newTrialEligibilityHandler(&stubLedger{exists: false})
	w := callTrialEligibility(h, `{"email_hash":"`+validHash+`"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	body := decodeTrialEligibilityBody(t, w)
	assert.Equal(t, true, body["eligible"])
}

// CR4-H-02: email already used → 409 TRIAL_ALREADY_USED.
func TestCheckTrialEligibility_AlreadyUsed(t *testing.T) {
	h := newTrialEligibilityHandler(&stubLedger{exists: true})
	w := callTrialEligibility(h, `{"email_hash":"`+validHash+`"}`)
	assertErrorCode(t, w, http.StatusConflict, "TRIAL_ALREADY_USED")
}

// CR4-H-03: missing email_hash field → 400 VALIDATION_ERROR.
func TestCheckTrialEligibility_MissingEmailHash(t *testing.T) {
	h := newTrialEligibilityHandler(&stubLedger{exists: false})
	w := callTrialEligibility(h, `{}`)
	assertErrorCode(t, w, http.StatusBadRequest, "VALIDATION_ERROR")
}

// CR4-H-03b: a hash that is not 64 hex characters → 400, never reaching
// the ledger (an oversized one would otherwise fail I-1's CHECK as a 500).
func TestCheckTrialEligibility_MalformedEmailHash(t *testing.T) {
	for _, h := range []string{"abc123", strings.Repeat("a", 129), strings.Repeat("z", 64)} {
		ledger := &stubLedger{}
		w := callTrialEligibility(newTrialEligibilityHandler(ledger), `{"email_hash":"`+h+`"}`)
		assertErrorCode(t, w, http.StatusBadRequest, "VALIDATION_ERROR")
		assert.Empty(t, ledger.checked, h)
	}
}

// CR4-H-03c: an upper-case spelling is checked as the lower-case hash, so
// it cannot bypass a used hash.
func TestCheckTrialEligibility_UpperCaseHashNormalized(t *testing.T) {
	ledger := &stubLedger{exists: true}
	w := callTrialEligibility(newTrialEligibilityHandler(ledger), `{"email_hash":"`+strings.ToUpper(validHash)+`"}`)
	assertErrorCode(t, w, http.StatusConflict, "TRIAL_ALREADY_USED")
	assert.Equal(t, []string{validHash}, ledger.checked)
}

// CR4-H-04: ledger DB outage → 503, never eligible:true.
func TestCheckTrialEligibility_DBUnavailable(t *testing.T) {
	h := newTrialEligibilityHandler(&stubLedger{err: domain.NewError(domain.ErrDBUnavailable, "db down")})
	w := callTrialEligibility(h, `{"email_hash":"`+validHash+`"}`)
	assertErrorCode(t, w, http.StatusServiceUnavailable, "DB_UNAVAILABLE")
}

// CR4-H-05: nil ledger (WithTrialLedger not called) → fail-open, 200 eligible:true.
func TestCheckTrialEligibility_NilLedger_FailOpen(t *testing.T) {
	svc := service.NewProvisioningService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h := &InternalHandler{provisioning: svc}
	w := callTrialEligibility(h, `{"email_hash":"`+validHash+`"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	body := decodeTrialEligibilityBody(t, w)
	assert.Equal(t, true, body["eligible"])
}

// CR4-AUTH-01: iam-system reaches the endpoint; commercial-system is 403
// (not on commercialSystemAllowedRoutes). Verified via the full router.
func TestCheckTrialEligibility_InternalCallerGate(t *testing.T) {
	const route = "/api/v1/internal/trial-eligibility"
	sys := simulateRoute(t, http.MethodPost, route, &requestctx.RequestContext{
		Roles: []string{"iam-system"}, UserID: uuid.Nil, TenantID: uuid.Nil,
	})
	assert.Equal(t, http.StatusOK, sys.Code, "iam-system must reach CR-4")

	com := simulateRoute(t, http.MethodPost, route, &requestctx.RequestContext{
		Roles: []string{"commercial-system"}, UserID: uuid.New(), TenantID: uuid.New(),
	})
	assert.Equal(t, http.StatusForbidden, com.Code, "commercial-system must be 403 on CR-4")
}
