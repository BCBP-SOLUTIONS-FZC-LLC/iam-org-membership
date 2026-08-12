// tools/mockwf — configurable mock for the Workflow Service internal API.
//
// O&M calls exactly three Workflow Service endpoints. This mock handles all
// three with the correct paths and response shapes (verified against both
// http_client.go and execution_openapi.yaml):
//
//	GET  /api/v1/internal/workflows/delegate-impact     → GetDelegateImpact
//	POST /api/v1/internal/workflows/reassign-delegate   → ReassignDelegate
//	POST /api/v1/internal/workflows/cancel-by-delegate  → CancelByDelegate
//
// ── Configuration (env vars) ─────────────────────────────────────────────
//
//	MOCK_WF_PORT            listen port                    (default: 8082)
//
//	# GetDelegateImpact (GET /delegate-impact)
//	MOCK_ACTIVE_WORKFLOWS   active_workflows count in 200  (default: 0)
//	MOCK_WF_IDS             comma-separated workflow UUIDs (default: "")
//	MOCK_FAIL_IMPACT        force this HTTP status code    (e.g. 503)
//
//	# ReassignDelegate (POST /reassign-delegate)
//	MOCK_REASSIGN_COUNT     reassigned count in 200 body   (default: 1)
//	MOCK_FAIL_REASSIGN      force this HTTP status code    (e.g. 503)
//
//	# CancelByDelegate (POST /cancel-by-delegate)
//	MOCK_CANCEL_COUNT       cancelled count in 200 body    (default: 1)
//	MOCK_FAIL_CANCEL        force this HTTP status code    (e.g. 503)
//
// ── Quick-start examples ─────────────────────────────────────────────────
//
//	# Happy path — user has NO active workflows (safe to remove):
//	MOCK_ACTIVE_WORKFLOWS=0 go run ./tools/mockwf/
//
//	# User has 2 active workflows → 409 workflow_resolution_required:
//	MOCK_ACTIVE_WORKFLOWS=2 MOCK_WF_IDS=wf-aaa-111,wf-bbb-222 go run ./tools/mockwf/
//
//	# Workflow Service is down → 503 (fail-open test):
//	MOCK_FAIL_IMPACT=503 go run ./tools/mockwf/
//
//	# Reassign fails → test 503 on P-26 replace_delegate:
//	MOCK_ACTIVE_WORKFLOWS=0 MOCK_FAIL_REASSIGN=503 go run ./tools/mockwf/
//
//	# Cancel fails → test 503 on P-26 stop_workflows:
//	MOCK_ACTIVE_WORKFLOWS=0 MOCK_FAIL_CANCEL=503 go run ./tools/mockwf/
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
)

func main() {
	port := envOr("MOCK_WF_PORT", "8082")

	mux := http.NewServeMux()

	// ── Endpoint registrations (paths match what O&M http_client.go calls) ──
	mux.HandleFunc("/api/v1/internal/workflows/delegate-impact", handleImpact)
	mux.HandleFunc("/api/v1/internal/workflows/reassign-delegate", handleReassign)
	mux.HandleFunc("/api/v1/internal/workflows/cancel-by-delegate", handleCancel)
	mux.HandleFunc("/", handleFallback)

	log.Printf("╔══════════════════════════════════════════════════")
	log.Printf("║  mock-wf  listening on :%s", port)
	log.Printf("╠══════════════════════════════════════════════════")
	log.Printf("║  MOCK_ACTIVE_WORKFLOWS = %s  (0 = safe to remove)", envOr("MOCK_ACTIVE_WORKFLOWS", "0"))
	log.Printf("║  MOCK_WF_IDS           = %s", envOr("MOCK_WF_IDS", "(auto-generated if count > 0)"))
	log.Printf("║  MOCK_FAIL_IMPACT      = %s", envOr("MOCK_FAIL_IMPACT", "(none — returns 200)"))
	log.Printf("║  MOCK_REASSIGN_COUNT   = %s  (workflows moved on reassign)", envOr("MOCK_REASSIGN_COUNT", "1"))
	log.Printf("║  MOCK_FAIL_REASSIGN    = %s", envOr("MOCK_FAIL_REASSIGN", "(none — returns 200)"))
	log.Printf("║  MOCK_CANCEL_COUNT     = %s  (workflows cancelled)", envOr("MOCK_CANCEL_COUNT", "1"))
	log.Printf("║  MOCK_FAIL_CANCEL      = %s", envOr("MOCK_FAIL_CANCEL", "(none — returns 200)"))
	log.Printf("╚══════════════════════════════════════════════════")

	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

// ── GET /api/v1/internal/workflows/delegate-impact ───────────────────────
//
// O&M sends: ?tenant_id=<uuid>&delegate_user_id=<uuid>[&delegation_id=<uuid>]
// O&M parses: { active_workflows: int, workflow_ids: []uuid }
//
// WF OpenAPI (execution_openapi.yaml §/workflows/active-by-user):
//
//	{
//	  "active_workflows": 2,              // count of active assignments
//	  "workflow_ids":     ["uuid1","uuid2"],// keyset-paginated preview
//	  "next_cursor":      "opaque"         // omitted when no more pages
//	}
func handleImpact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	log.Printf("[mock-wf] GET  /delegate-impact  tenant=%s  user=%s  delegation=%s",
		q.Get("tenant_id"), q.Get("delegate_user_id"), q.Get("delegation_id"))

	if code := failCode("MOCK_FAIL_IMPACT"); code != 0 {
		writeError(w, code, "mock_forced_failure — simulating WF service error")
		log.Printf("[mock-wf] => forced %d (MOCK_FAIL_IMPACT)", code)
		return
	}

	active := envInt("MOCK_ACTIVE_WORKFLOWS", 0)
	ids := wfIDs()
	// Auto-generate placeholder UUIDs when count > 0 but no explicit IDs given
	if len(ids) == 0 && active > 0 {
		for i := range active {
			ids = append(ids, fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1))
		}
	}

	resp := map[string]any{
		"active_workflows": active,
		"workflow_ids":     ids,
	}
	if active > len(ids) && len(ids) > 0 {
		// Simulate pagination cursor when total > page
		resp["next_cursor"] = "mock-next-page-cursor"
	}

	writeJSON(w, http.StatusOK, resp)
	log.Printf("[mock-wf] => 200  active_workflows=%d  workflow_ids=%v", active, ids)
}

// ── POST /api/v1/internal/workflows/reassign-delegate ────────────────────
//
// O&M sends body: { tenant_id, old_delegate_id, new_delegate_id [, delegation_id] }
// WF OpenAPI response: { "reassigned": <count_of_workflows_moved> }
func handleReassign(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body := readBody(r)
	log.Printf("[mock-wf] POST /reassign-delegate  body=%s", body)

	if code := failCode("MOCK_FAIL_REASSIGN"); code != 0 {
		writeError(w, code, "mock_forced_failure — simulating WF service error on reassign")
		log.Printf("[mock-wf] => forced %d (MOCK_FAIL_REASSIGN)", code)
		return
	}

	count := envInt("MOCK_REASSIGN_COUNT", 1)
	writeJSON(w, http.StatusOK, map[string]any{"reassigned": count})
	log.Printf("[mock-wf] => 200  reassigned=%d", count)
}

// ── POST /api/v1/internal/workflows/cancel-by-delegate ───────────────────
//
// O&M sends body: { tenant_id, delegate_user_id [, delegation_id] }
// WF OpenAPI response: { "cancelled": <count_of_workflows_cancelled> }
func handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body := readBody(r)
	log.Printf("[mock-wf] POST /cancel-by-delegate  body=%s", body)

	if code := failCode("MOCK_FAIL_CANCEL"); code != 0 {
		writeError(w, code, "mock_forced_failure — simulating WF service error on cancel")
		log.Printf("[mock-wf] => forced %d (MOCK_FAIL_CANCEL)", code)
		return
	}

	count := envInt("MOCK_CANCEL_COUNT", 1)
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": count})
	log.Printf("[mock-wf] => 200  cancelled=%d", count)
}

// handleFallback logs any unmatched request — useful for debugging path mismatches.
func handleFallback(w http.ResponseWriter, r *http.Request) {
	log.Printf("[mock-wf] ⚠️  UNMATCHED %s %s — check that O&M WORKFLOW_SERVICE_BASE_URL points here", r.Method, r.URL.Path)
	writeError(w, http.StatusNotFound, fmt.Sprintf("mock-wf: no handler for %s %s", r.Method, r.URL.Path))
}

// ── helpers ───────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg, "code": code})
}

func readBody(r *http.Request) string {
	if r.Body == nil {
		return "(empty)"
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	return string(b)
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

func failCode(key string) int { return envInt(key, 0) }

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
