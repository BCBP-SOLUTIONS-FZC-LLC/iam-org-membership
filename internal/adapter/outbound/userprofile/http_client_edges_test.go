// Phase 19 — userprofile HTTP client edge branches:
//   - NewHTTPClient nil-logger + non-positive timeout defaults
//   - SetAvailability request-build error (bad baseURL)
//   - SetAvailability empty baseURL → configured-error
package userprofile

import (
	"context"
	"log/slog"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestP19UPEdges_NewHTTPClient_DefaultsOnZeroTimeoutAndNilLogger(t *testing.T) {
	c := NewHTTPClient("http://x", 0, nil)
	require.NotNil(t, c)
	assert.NotNil(t, c.logger)
	assert.NotZero(t, c.client.Timeout)
}

func TestP19UPEdges_SetAvailability_EmptyBaseURL_ReturnsError(t *testing.T) {
	c := NewHTTPClient("", 0, slog.Default())
	err := c.SetAvailability(context.Background(), port.SetAvailabilityRequest{
		TenantID: uuid.New(), UserID: uuid.New(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "baseURL not configured")
}

func TestP19UPEdges_SetAvailability_BadBaseURL_RequestBuildError(t *testing.T) {
	c := NewHTTPClient("http://\x7f", 0, slog.Default())
	err := c.SetAvailability(context.Background(), port.SetAvailabilityRequest{
		TenantID: uuid.New(), UserID: uuid.New(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "build availability request")
}
