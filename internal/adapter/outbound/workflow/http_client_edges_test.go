// Phase 19 — workflow HTTP client edge branches:
//   - NewHTTPClient nil-logger + non-positive timeout defaults
//   - GetDelegateImpact malformed-JSON decode error, request-build error, non-2xx status
//   - CancelByDelegate delegationID != nil body branch + transport error
//   - postInternal 2xx pass-through
package workflow

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestP19WFEdges_NewHTTPClient_NilLoggerAndZeroTimeout(t *testing.T) {
	c := NewHTTPClient("http://x", 0, nil)
	require.NotNil(t, c)
	assert.Nil(t, c.logger, "nil logger is a no-op; warn() must not panic")
	assert.Equal(t, "http://x", c.baseURL)
	// The client timeout should have been defaulted to 3s.
	assert.NotZero(t, c.client.Timeout)
}

func TestP19WFEdges_GetDelegateImpact_MalformedJSON_ReturnsDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("this is not JSON"))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, nil)
	impact, err := c.GetDelegateImpact(context.Background(), uuid.New(), uuid.New(), nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "decode")
	assert.Nil(t, impact)
}

func TestP19WFEdges_GetDelegateImpact_Non2xx_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, nil)
	_, err := c.GetDelegateImpact(context.Background(), uuid.New(), uuid.New(), nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestP19WFEdges_GetDelegateImpact_RequestBuildError_BadBaseURL(t *testing.T) {
	// Control-character in baseURL forces http.NewRequestWithContext to error.
	c := NewHTTPClient("http://\x7f", 0, nil)
	_, err := c.GetDelegateImpact(context.Background(), uuid.New(), uuid.New(), nil)
	assert.Error(t, err)
}

func TestP19WFEdges_CancelByDelegate_WithDelegationID_TransportError(t *testing.T) {
	// srv closes immediately → transport error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	c := NewHTTPClient(srv.URL, 0, nil)
	dgID := uuid.New()
	err := c.CancelByDelegate(context.Background(), uuid.New(), uuid.New(), &dgID)
	assert.Error(t, err)
}

func TestP19WFEdges_CancelByDelegate_Success_WithDelegationID(t *testing.T) {
	// Exercises the "delegationID != nil" body branch on postInternal happy path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, nil)
	dgID := uuid.New()
	err := c.CancelByDelegate(context.Background(), uuid.New(), uuid.New(), &dgID)
	assert.NoError(t, err)
}

func TestP19WFEdges_ReassignDelegate_Success_WithDelegationID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 0, nil)
	dgID := uuid.New()
	err := c.ReassignDelegate(context.Background(), uuid.New(), uuid.New(), uuid.New(), &dgID)
	assert.NoError(t, err)
}
