// Phase 19 — groupmappingclient HTTP client JSON-decode error branch.
//
// Covers the path where the server returns 200 OK with Content-Type:
// application/json but an invalid JSON body, triggering the
// json.NewDecoder(...).Decode(&out) error return in ResolveGroups (GM-I1).
package groupmappingclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveGroups_MalformedJSON_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not valid json"))
	}))
	defer srv.Close()

	tenantID := uuid.New()
	c := NewHTTPClient(srv.URL, 5*time.Second, nil)
	_, err := c.ResolveGroups(context.Background(), tenantID, []string{"group1"})
	require.Error(t, err)
	assert.Error(t, err, "ResolveGroups must surface the JSON decode error")
}
