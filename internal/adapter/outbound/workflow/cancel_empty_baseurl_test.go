package workflow

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCancelByDelegate_EmptyBaseURL_IsNoop verifies the `if c.baseURL == ""`
// early-return branch (lines 149-152): when the workflow service URL is
// unconfigured, CancelByDelegate logs and returns nil — never errors.
func TestCancelByDelegate_EmptyBaseURL_IsNoop(t *testing.T) {
	c := NewHTTPClient("", 0, nil)

	err := c.CancelByDelegate(context.Background(), uuid.New(), uuid.New(), nil)

	require.NoError(t, err, "empty baseURL must be a silent no-op")
}

// TestCancelByDelegate_EmptyBaseURL_WithDelegationID_IsNoop verifies the same
// branch when delegationID is non-nil (both code paths still hit baseURL=="").
func TestCancelByDelegate_EmptyBaseURL_WithDelegationID_IsNoop(t *testing.T) {
	c := NewHTTPClient("", 0, nil)
	delID := uuid.New()

	err := c.CancelByDelegate(context.Background(), uuid.New(), uuid.New(), &delID)

	require.NoError(t, err, "empty baseURL with delegationID must also be a no-op")
}

// TestPostInternal_BadURL_RequestBuildError_Propagates covers the
// http.NewRequestWithContext error path in postInternal (line 169-171):
// a malformed URL (contains control character) causes the request build to fail.
func TestPostInternal_BadURL_RequestBuildError_Propagates(t *testing.T) {
	// "\x7f" is a control character that makes http.NewRequest return an error.
	c := NewHTTPClient("http://host\x7f", 0, nil)

	err := c.CancelByDelegate(context.Background(), uuid.New(), uuid.New(), nil)

	assert.Error(t, err, "bad URL must cause request-build error to propagate")
}
