// tools/mockwf — configurable mock for the Workflow Service internal API.
//
// Handles the three endpoints iam-org-membership calls:
//
//	GET  /api/v1/internal/workflows/active-by-user   (GetDelegateImpact)
//	POST /api/v1/internal/workflows/reassign          (ReassignDelegate)
//	POST /api/v1/internal/workflows/cancel-by-delegate (CancelByDelegate)
//
// Configuration (env vars):
//
//	MOCK_WF_PORT           — listen port (default 8082)
//	MOCK_ACTIVE_WORKFLOWS  — active_workflows count returned by GET (default 0)
//	MOCK_WF_IDS            — comma-separated workflow UUIDs returned by GET (default "")
//	MOCK_FAIL_IMPACT       — HTTP status to return on GET instead of 200 (e.g. 503)
//	MOCK_FAIL_REASSIGN     — HTTP status to return on POST reassign instead of 200
//	MOCK_FAIL_CANCEL       — HTTP status to return on POST cancel instead of 200
//
// Usage:
//
//	MOCK_ACTIVE_WORKFLOWS=0 go run ./tools/mockwf/
//	MOCK_ACTIVE_WORKFLOWS=2 MOCK_WF_IDS=wf-id-1,wf-id-2 go run ./tools/mockwf/
//	MOCK_FAIL_IMPACT=503    go run ./tools/mockwf/
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
)

func main() {
	port := envOr("MOCK_WF_PORT", "8082")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/internal/workflows/active-by-user", handleImpact)
	mux.HandleFunc("/api/v1/internal/workflows/reassign", handleReassign)
	mux.HandleFunc("/api/v1/internal/workflows/cancel-by-delegate", handleCancel)
	mux.HandleFunc("/", handleFallback)

	log.Printf("mock-wf listening on :%s", port)
	log.Printf("  MOCK_ACTIVE_WORKFLOWS = %s", envOr("MOCK_ACTIVE_WORKFLOWS", "0"))
	log.Printf("  MOCK_WF_IDS           = %s", envOr("MOCK_WF_IDS", "(none)"))
	log.Printf("  MOCK_FAIL_IMPACT      = %s", envOr("MOCK_FAIL_IMPACT", "(none — 200)"))
	log.Printf("  MOCK_FAIL_REASSIGN    = %s", envOr("MOCK_FAIL_REASSIGN", "(none — 200)"))
	log.Printf("  MOCK_FAIL_CANCEL      = %s", envOr("MOCK_FAIL_CANCEL", "(none — 200)"))

	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

// GET /api/v1/internal/workflows/active-by-user
func handleImpact(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	log.Printf("[mock-wf] GET active-by-user  tenant_id=%s user_id=%s delegation_id=%s",
		q.Get("tenant_id"), q.Get("user_id"), q.Get("delegation_id"))

	if code := failCode("MOCK_FAIL_IMPACT"); code != 0 {
		log.Printf("[mock-wf] => forcing %d", code)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, `{"error":"mock_forced_failure","code":%d}`, code)
		return
	}

	active := envInt("MOCK_ACTIVE_WORKFLOWS", 0)
	ids := wfIDs()
	if len(ids) == 0 && active > 0 {
		for i := range active {
			ids = append(ids, fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	resp := map[string]any{
		"active_workflows": active,
		"workflow_ids":     ids,
	}
	_ = json.NewEncoder(w).Encode(resp)
	log.Printf("[mock-wf] => 200 active_workflows=%d workflow_ids=%v", active, ids)
}

// POST /api/v1/internal/workflows/reassign
func handleReassign(w http.ResponseWriter, r *http.Request) {
	log.Printf("[mock-wf] POST reassign")
	if code := failCode("MOCK_FAIL_REASSIGN"); code != 0 {
		log.Printf("[mock-wf] => forcing %d", code)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, `{"error":"mock_forced_failure","code":%d}`, code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, `{"reassigned":true}`)
	log.Printf("[mock-wf] => 200 OK")
}

// POST /api/v1/internal/workflows/cancel-by-delegate
func handleCancel(w http.ResponseWriter, r *http.Request) {
	log.Printf("[mock-wf] POST cancel-by-delegate")
	if code := failCode("MOCK_FAIL_CANCEL"); code != 0 {
		log.Printf("[mock-wf] => forcing %d", code)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, `{"error":"mock_forced_failure","code":%d}`, code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, `{"cancelled":true}`)
	log.Printf("[mock-wf] => 200 OK")
}

func handleFallback(w http.ResponseWriter, r *http.Request) {
	log.Printf("[mock-wf] UNMATCHED %s %s", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprintf(w, `{"error":"not_found","path":"%s"}`, r.URL.Path)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func failCode(key string) int {
	return envInt(key, 0)
}

func wfIDs() []string {
	raw := os.Getenv("MOCK_WF_IDS")
	if raw == "" {
		return []string{}
	}
	parts := strings.Split(raw, ",")
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
