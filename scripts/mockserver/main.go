//go:build ignore

//cd /Users/sharmila/bcbp-solutions/XpertPMS/Org-Membership/iam-org-membership
//make mock-servers

// mockserver starts lightweight HTTP stubs for the three external services
// that iam-org-membership calls during manual testing:
//
//   - :8084  Catalog Admin       (CATALOG_ADMIN_BASE_URL)      — NOT fail-open
//   - :8083  Realm Provisioner   (REALM_PROVISIONER_BASE_URL)  — P-6 invite path
//   - :8082  Workflow Service    (WORKFLOW_SERVICE_BASE_URL)    — P-8 remove / P-26 resolution
//
// Run with:
//
//	go run scripts/mockserver/main.go
//	go run scripts/mockserver/main.go -workflow-active 2   # simulate active workflows on P-8
//
// Zero production code is imported — pure stdlib only.
// The //go:build ignore tag keeps this file invisible to go build ./... ,
// go test ./... , go vet , and golangci-lint.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ── flags ────────────────────────────────────────────────────────────────────

var (
	workflowActive = flag.Int("workflow-active", 0,
		"number of active_workflows returned by Workflow /delegate-impact (default 0 = allow removal)")
	catalogPort  = flag.String("catalog-port", "8084", "Catalog Admin listen port")
	rpPort       = flag.String("rp-port", "8083", "Realm Provisioner listen port")
	workflowPort = flag.String("workflow-port", "8082", "Workflow Service listen port")
	rpMFAFail    = flag.Bool("rp-mfa-fail", false, "make RP return 503 on mfa-reset (simulate DEP failure for P34-DEP-01/02)")
)

// mfaResetCalls tracks every mfa-reset RP call received (tenant_id/user_id).
// Printed to stdout so manual tests can verify RP-9 was invoked.
var mfaResetCalls []string
var mfaResetMu sync.Mutex

func recordMFAReset(tenantID, userID string) {
	mfaResetMu.Lock()
	mfaResetCalls = append(mfaResetCalls, fmt.Sprintf("tenant=%s user=%s", tenantID, userID))
	mfaResetMu.Unlock()
	log.Printf("🔑 [RP] mfa-reset called: tenant=%s user=%s  (total calls so far: %d)", tenantID, userID, len(mfaResetCalls))
}

// ── helpers ──────────────────────────────────────────────────────────────────

func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:]))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func logReq(service, method, path string) {
	log.Printf("[%-16s] %s %s", service, method, path)
}

func mux(service string, routes map[string]http.HandlerFunc) http.Handler {
	m := http.NewServeMux()
	for pattern, h := range routes {
		handler := h
		pat := pattern
		m.HandleFunc(pat, func(w http.ResponseWriter, r *http.Request) {
			logReq(service, r.Method, r.URL.Path)
			handler(w, r)
		})
	}
	return m
}

func serve(port, service string, handler http.Handler, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		addr := ":" + port
		// Bind the port first so the log line is accurate.
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Printf("❌ %-24s port %s already in use — run: lsof -nP -iTCP:%s | grep LISTEN  then kill that PID", service, port, port)
			return
		}
		log.Printf("🟢 %-24s listening on %s", service, addr)
		srv := &http.Server{Handler: handler}
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("❌ %-24s error: %v", service, err)
		}
	}()
}

// ════════════════════════════════════════════════════════════════════════════
// Catalog Admin mock  —  :8084
//
// Endpoints:
//   GET /api/v1/internal/plans        (CAT-I2)
//   GET /api/v1/internal/departments  (CAT-I1)
//
// Fixed department UUIDs are stable across restarts so that department_ids
// written to tenant_departments during I-1 TrialSignup remain resolvable
// on subsequent DepartmentByID calls (P-24, P-25, P-10).
// ════════════════════════════════════════════════════════════════════════════

// All IDs use only valid hex characters (0-9, a-f).
// These are stable across restarts so department_ids written to tenant_departments
// during I-1 TrialSignup remain resolvable on subsequent DepartmentByID calls.
var fixedDepartments = []map[string]any{
	{"id": "de010001-0000-0000-0000-000000000001", "code": "engineering", "name": "Engineering", "is_system": true, "is_active": true, "record_version": 1},
	{"id": "de010002-0000-0000-0000-000000000002", "code": "design", "name": "Design", "is_system": true, "is_active": true, "record_version": 1},
	{"id": "de010003-0000-0000-0000-000000000003", "code": "procurement", "name": "Procurement", "is_system": true, "is_active": true, "record_version": 1},
	{"id": "de010004-0000-0000-0000-000000000004", "code": "finance", "name": "Finance", "is_system": true, "is_active": true, "record_version": 1},
	{"id": "de010005-0000-0000-0000-000000000005", "code": "legal", "name": "Legal", "is_system": true, "is_active": true, "record_version": 1},
	{"id": "de010006-0000-0000-0000-000000000006", "code": "operations", "name": "Operations", "is_system": false, "is_active": true, "record_version": 1},
	{"id": "de010007-0000-0000-0000-000000000007", "code": "archive", "name": "Archive", "is_system": false, "is_active": false, "record_version": 1},
	{"id": "de010008-0000-0000-0000-000000000008", "code": "logistics", "name": "Logistics", "is_system": false, "is_active": true, "record_version": 1},
	{"id": "de010009-0000-0000-0000-000000000009", "code": "accounting", "name": "Accounting", "is_system": false, "is_active": true, "record_version": 1},
}

var fixedPlans = []map[string]any{
	{
		"code": "starter", "display_name": "Starter",
		"trial_duration_days": 30, "sso_enabled": false,
		"custom_branding": "none", "feature_set": map[string]any{},
		"record_version": 1,
	},
	{
		"code": "pro", "display_name": "Professional",
		"trial_duration_days": 30, "sso_enabled": true,
		"custom_branding": "logo", "feature_set": map[string]any{"advanced_reporting": true},
		"record_version": 1,
	},
	{
		"code": "enterprise", "display_name": "Enterprise",
		"trial_duration_days": 30, "sso_enabled": true,
		"custom_branding": "logo", "feature_set": map[string]any{"advanced_reporting": true, "custom_roles": true},
		"record_version": 1,
	},
}

func catalogHandler() http.Handler {
	routes := map[string]http.HandlerFunc{
		"/api/v1/internal/plans": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"plans":           fixedPlans,
				"record_versions": map[string]int{"starter": 1, "pro": 1, "enterprise": 1},
			})
		},
		"/api/v1/internal/departments": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"departments": fixedDepartments,
				"as_of":       time.Now().UTC().Format(time.RFC3339),
			})
		},
	}
	return mux("catalog-admin", routes)
}

// ════════════════════════════════════════════════════════════════════════════
// Realm Provisioner mock  —  :8083
//
// Endpoints:
//   POST   /api/v1/internal/users/invite               → CreateInvitedUser (P-6)
//   DELETE /api/v1/internal/users/{kc_user_id}         → DeleteUser (PI-9)
//   PATCH  /api/v1/internal/tenants/{id}/realm-config  → PatchRealmConfig (P-2/T-15)
//   POST   /api/v1/internal/users/{id}/revoke-sessions → RevokeUserSessions (P-7/P-8)
// ════════════════════════════════════════════════════════════════════════════

func realmProvisionerHandler() http.Handler {
	mux := http.NewServeMux()

	// POST /api/v1/internal/users/invite  → CreateInvitedUser
	mux.HandleFunc("/api/v1/internal/users/invite", func(w http.ResponseWriter, r *http.Request) {
		logReq("realm-provisioner", r.Method, r.URL.Path)
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"keycloak_user_id": randomUUID(),
		})
	})

	// /api/v1/internal/users/{id}  — DELETE = DeleteUser, POST */revoke-sessions = RevokeUserSessions
	mux.HandleFunc("/api/v1/internal/users/", func(w http.ResponseWriter, r *http.Request) {
		logReq("realm-provisioner", r.Method, r.URL.Path)
		path := r.URL.Path

		switch {
		// POST /api/v1/internal/users/{id}/revoke-sessions
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/revoke-sessions"):
			w.WriteHeader(http.StatusOK)

		// DELETE /api/v1/internal/users/{id}
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	// PATCH /api/v1/internal/tenants/{id}/realm-config  → PatchRealmConfig
	// POST  /api/v1/internal/tenants/{id}/users/{uid}/mfa-reset → ResetMFA (RP-9, P-34)
	mux.HandleFunc("/api/v1/internal/tenants/", func(w http.ResponseWriter, r *http.Request) {
		logReq("realm-provisioner", r.Method, r.URL.Path)
		path := r.URL.Path

		// POST .../users/{uid}/mfa-reset  — ResetMFA (RP-9)
		if r.Method == http.MethodPost && strings.HasSuffix(path, "/mfa-reset") {
			// Extract tenant and user IDs from path for logging
			// path: /api/v1/internal/tenants/{tid}/users/{uid}/mfa-reset
			parts := strings.Split(strings.TrimPrefix(path, "/api/v1/internal/tenants/"), "/")
			tenantID, userID := "", ""
			if len(parts) >= 1 {
				tenantID = parts[0]
			}
			if len(parts) >= 3 {
				userID = parts[2]
			}
			recordMFAReset(tenantID, userID)
			if *rpMFAFail {
				log.Printf("⚠️  [RP] rp-mfa-fail=true → returning 503 (P34-DEP-01/02 simulation)")
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "realm_provisioner_unavailable"})
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// PATCH .../realm-config  — PatchRealmConfig
		if r.Method != http.MethodPatch {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	return mux
}

// ════════════════════════════════════════════════════════════════════════════
// Workflow Service mock  —  :8082
//
// Endpoints:
//   GET  /api/v1/internal/workflows/delegate-impact     → GetDelegateImpact (P-8/P-11)
//   POST /api/v1/internal/workflows/reassign-delegate   → ReassignDelegate (P-26)
//   POST /api/v1/internal/workflows/cancel-by-delegate  → CancelByDelegate (P-26)
//
// Use -workflow-active N to simulate active workflows (makes P-8 return 409).
// ════════════════════════════════════════════════════════════════════════════

func workflowHandler(activeCount int) http.Handler {
	routes := map[string]http.HandlerFunc{
		"/api/v1/internal/workflows/delegate-impact": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			workflowIDs := []string{}
			if activeCount > 0 {
				for i := 0; i < activeCount; i++ {
					workflowIDs = append(workflowIDs, randomUUID())
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"active_workflows": activeCount,
				"workflow_ids":     workflowIDs,
			})
		},
		"/api/v1/internal/workflows/reassign-delegate": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.WriteHeader(http.StatusOK)
		},
		"/api/v1/internal/workflows/cancel-by-delegate": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.WriteHeader(http.StatusOK)
		},
	}
	return mux("workflow", routes)
}

// ════════════════════════════════════════════════════════════════════════════
// main
// ════════════════════════════════════════════════════════════════════════════

func main() {
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("")

	fmt.Println(`
╔══════════════════════════════════════════════════════╗
║       iam-org-membership  mock servers               ║
╠══════════════════════════════════════════════════════╣
║  :8084  Catalog Admin      /api/v1/internal/plans    ║
║                            /api/v1/internal/depts    ║
║  :8083  Realm Provisioner  /api/v1/internal/users/*  ║
║  :8082  Workflow Service   /api/v1/internal/wf/*     ║
╠══════════════════════════════════════════════════════╣
║  Ctrl-C to stop                                      ║
╚══════════════════════════════════════════════════════╝
`)

	if *workflowActive > 0 {
		log.Printf("⚠️  workflow-active=%d → P-8 DELETE will return 409 workflow_resolution_required", *workflowActive)
	}

	var wg sync.WaitGroup
	serve(*catalogPort, "catalog-admin   :"+*catalogPort, catalogHandler(), &wg)
	serve(*rpPort, "realm-provisioner:"+*rpPort, realmProvisionerHandler(), &wg)
	serve(*workflowPort, "workflow         :"+*workflowPort, workflowHandler(*workflowActive), &wg)

	wg.Wait()
}
