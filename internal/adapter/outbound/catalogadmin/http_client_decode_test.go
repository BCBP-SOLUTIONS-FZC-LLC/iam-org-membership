// Phase 19 — catalogadmin HTTP client JSON-decode error branches.
//
// Covers the path where the server returns 200 OK with Content-Type:
// application/json but an invalid JSON body, triggering the
// json.NewDecoder(...).Decode(&out) error return in both Departments (CAT-I1)
// and Plans (CAT-I2).
package catalogadmin

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDepartments_MalformedJSON_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("this is not valid json {{{"))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	_, err := c.Departments(context.Background())
	require.Error(t, err)
	assert.Error(t, err, "Departments must surface the JSON decode error")
}

func TestPlans_MalformedJSON_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("this is not valid json {{{"))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, 5*time.Second, slog.Default())
	_, err := c.Plans(context.Background())
	require.Error(t, err)
	assert.Error(t, err, "Plans must surface the JSON decode error")
}
