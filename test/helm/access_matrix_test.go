//go:build helm

// Package helm_test — CD-19 access-matrix assertions over the rendered Helm
// chart (LLD §10.2, rev 2.40).
//
// helm-check.sh proves the chart renders and that standard resources are
// schema-valid, but not WHICH routes each cross-namespace caller can reach.
// This test renders the chart exactly as helm-check.sh does (same --set
// stubs) with the staging and prod overlays, parses the YAML, and pins:
//
//   - Istio AuthorizationPolicy (L7): exactly one ALLOW policy whose rules are
//     the namespace rule [iam, gateway, observability] (no path limit), the
//     workflow-app namespace rule (POST */assignee-override, GET */exists,
//     */eligibility) and three principal rules — Billing (GET */seat-usage,
//     */plan-change-check, notPaths /api/v1/tenants/*), Entitlements (GET
//     */plan-context) and the Signup BFF (POST /api/v1/internal/tenants and
//     /api/v1/internal/trial-eligibility, GET /api/v1/public/tenants/resolve).
//     Nothing else.
//   - NetworkPolicy (L4): the billing, commercial and bff namespaces are
//     admitted only together with their caller's pod labels, on the API port
//     only; no rule admits them (or every namespace) by namespace alone.
//
// The app layer (L7 inside the service) is pinned separately by
// TestCommercialSystemAllowedRoutes_StableKeys
// (internal/adapter/inbound/http/middleware_cd19_test.go): the
// commercial-system principal may reach I-11, I-17 and I-18 only.
//
// Run: make helm-access-check (or go test -tags=helm ./test/helm/...).
// Requires the helm binary on PATH; CI installs it in helm-check.sh.
package helm_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	releaseName = "iam-org-membership"
	namespace   = "iam"
	apiPort     = 8080
	trustDomain = "cluster.local"
)

// Same overrides as SHARED_SETS in .github/scripts/helm-check.sh.
var sharedSets = []string{"--set", "image.tag=ci-check", "--set", "existingSecret=ci-lint-skip"}

// op is one normalised Istio `to.operation`.
type op struct {
	Methods  []string
	Paths    []string
	NotPaths []string
}

func (o op) key() string {
	return strings.Join(o.Methods, ",") + "|" + strings.Join(o.Paths, ",") + "|" + strings.Join(o.NotPaths, ",")
}

// Expected principal rules (SPIFFE principal → operations).
var wantPrincipals = map[string][]op{
	trustDomain + "/ns/billing/sa/commercial-billing": {
		{Methods: []string{"GET"}, Paths: []string{"*/plan-change-check", "*/seat-usage"}, NotPaths: []string{"/api/v1/tenants/*"}},
	},
	trustDomain + "/ns/commercial/sa/commercial-entitlements": {
		{Methods: []string{"GET"}, Paths: []string{"*/plan-context"}},
	},
	trustDomain + "/ns/bff/sa/bff-signup": {
		{Methods: []string{"GET"}, Paths: []string{"/api/v1/public/tenants/resolve"}},
		{Methods: []string{"POST"}, Paths: []string{"/api/v1/internal/tenants", "/api/v1/internal/trial-eligibility"}},
	},
}

// The workflow-app namespace rule — path-limited.
var wantWorkflowOps = []op{
	{Methods: []string{"GET"}, Paths: []string{"*/eligibility", "*/exists"}},
	{Methods: []string{"POST"}, Paths: []string{"*/assignee-override"}},
}

// The existing-callers namespace rule — the only rule with no path limit.
var wantExistingCallerNamespaces = []string{"gateway", "iam", "observability"}

// Namespaces that may only ever be admitted per pod (NetworkPolicy), with
// the pod label each must carry.
var podScopedNamespaces = map[string]string{
	"billing":    "commercial-billing",
	"commercial": "commercial-entitlements",
	"bff":        "bff-signup",
}

func TestAccessMatrix_Staging(t *testing.T) { checkAccessMatrix(t, "values-staging.yaml") }
func TestAccessMatrix_Prod(t *testing.T)    { checkAccessMatrix(t, "values-prod.yaml") }

func checkAccessMatrix(t *testing.T, overlay string) {
	t.Helper()
	docs := render(t, overlay)

	var aps, nps []map[string]any
	for _, d := range docs {
		switch d["kind"] {
		case "AuthorizationPolicy":
			aps = append(aps, d)
		case "NetworkPolicy":
			nps = append(nps, d)
		}
	}
	if len(aps) != 1 {
		t.Fatalf("[%s] want exactly 1 Istio AuthorizationPolicy, got %d (is commercialCallers.istio.enabled true?)", overlay, len(aps))
	}
	if len(nps) != 1 {
		t.Fatalf("[%s] want exactly 1 NetworkPolicy, got %d", overlay, len(nps))
	}
	checkAuthorizationPolicy(t, overlay, aps[0])
	checkNetworkPolicy(t, overlay, nps[0])
}

// ── Istio AuthorizationPolicy ────────────────────────────────────────────

func checkAuthorizationPolicy(t *testing.T, overlay string, ap map[string]any) {
	t.Helper()
	spec := asMap(ap["spec"])
	if spec["action"] != "ALLOW" {
		t.Errorf("[%s] AuthorizationPolicy action = %v, want ALLOW", overlay, spec["action"])
	}

	gotPrincipals := map[string][]op{}
	var gotWorkflow []op
	existingRules := 0

	for i, r := range asSlice(spec["rules"]) {
		rule := asMap(r)
		for k := range rule {
			if k != "from" && k != "to" {
				t.Errorf("[%s] rule %d: unexpected key %q (only from/to are pinned)", overlay, i, k)
			}
		}
		froms := asSlice(rule["from"])
		if len(froms) != 1 {
			t.Errorf("[%s] rule %d: want exactly one `from` (a rule without one admits every source), got %d", overlay, i, len(froms))
			continue
		}
		src := asMap(asMap(froms[0])["source"])
		for k := range src {
			if k != "principals" && k != "namespaces" {
				t.Errorf("[%s] rule %d: unexpected source key %q", overlay, i, k)
			}
		}
		ops := normaliseOps(t, overlay, i, rule["to"])
		principals := strs(src["principals"])
		namespaces := strs(src["namespaces"])

		switch {
		case len(principals) > 0 && len(namespaces) == 0:
			if len(principals) != 1 {
				t.Errorf("[%s] rule %d: want one principal per rule, got %v", overlay, i, principals)
				continue
			}
			p := principals[0]
			if _, dup := gotPrincipals[p]; dup {
				t.Errorf("[%s] principal %q appears in more than one rule", overlay, p)
			}
			gotPrincipals[p] = ops
		case len(namespaces) > 0 && len(principals) == 0:
			sort.Strings(namespaces)
			switch {
			case reflect.DeepEqual(namespaces, []string{"workflow-app"}):
				if gotWorkflow != nil {
					t.Errorf("[%s] workflow-app appears in more than one rule", overlay)
				}
				gotWorkflow = ops
			case reflect.DeepEqual(namespaces, wantExistingCallerNamespaces):
				existingRules++
				if len(ops) != 0 {
					t.Errorf("[%s] existing-callers rule must carry no `to` (it mirrors the base NetworkPolicy), got %v", overlay, ops)
				}
			default:
				t.Errorf("[%s] rule %d admits namespaces %v — only %v (unrestricted) and [workflow-app] (path-limited) are allowed",
					overlay, i, namespaces, wantExistingCallerNamespaces)
			}
		default:
			t.Errorf("[%s] rule %d: source must name principals or namespaces, not both/neither: %v", overlay, i, src)
		}
	}

	if existingRules != 1 {
		t.Errorf("[%s] want exactly one existing-callers namespace rule %v, got %d", overlay, wantExistingCallerNamespaces, existingRules)
	}
	if !reflect.DeepEqual(gotWorkflow, wantWorkflowOps) {
		t.Errorf("[%s] workflow-app operations:\n got  %+v\n want %+v", overlay, gotWorkflow, wantWorkflowOps)
	}
	for p, want := range wantPrincipals {
		got, ok := gotPrincipals[p]
		if !ok {
			t.Errorf("[%s] principal %q has no rule", overlay, p)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("[%s] principal %q operations:\n got  %+v\n want %+v", overlay, p, got, want)
		}
	}
	for p := range gotPrincipals {
		if _, ok := wantPrincipals[p]; !ok {
			t.Errorf("[%s] unexpected principal %q is admitted", overlay, p)
		}
	}
}

// normaliseOps turns a rule's `to` list into sorted ops, rejecting any
// operation key other than methods/paths/notPaths (ports, hosts, notMethods…
// would change the matrix without this test noticing).
func normaliseOps(t *testing.T, overlay string, rule int, to any) []op {
	t.Helper()
	var out []op
	for _, e := range asSlice(to) {
		o := asMap(asMap(e)["operation"])
		for k := range o {
			if k != "methods" && k != "paths" && k != "notPaths" {
				t.Errorf("[%s] rule %d: unexpected operation key %q", overlay, rule, k)
			}
		}
		n := op{Methods: sorted(strs(o["methods"])), Paths: sorted(strs(o["paths"])), NotPaths: sorted(strs(o["notPaths"]))}
		if len(n.Methods) == 0 || len(n.Paths) == 0 {
			t.Errorf("[%s] rule %d: operation without methods or paths admits too much: %+v", overlay, rule, o)
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// ── NetworkPolicy ────────────────────────────────────────────────────────

func checkNetworkPolicy(t *testing.T, overlay string, np map[string]any) {
	t.Helper()
	admitted := map[string]bool{}
	for i, r := range asSlice(asMap(np["spec"])["ingress"]) {
		rule := asMap(r)
		froms := asSlice(rule["from"])
		if len(froms) == 0 {
			t.Errorf("[%s] ingress rule %d has no `from` — it admits every source", overlay, i)
			continue
		}
		for _, f := range froms {
			peer := asMap(f)
			if _, ok := peer["ipBlock"]; ok {
				t.Errorf("[%s] ingress rule %d: ipBlock peers are not expected", overlay, i)
			}
			nsSel, hasNS := peer["namespaceSelector"]
			if !hasNS {
				continue // podSelector only = same namespace (iam)
			}
			nsLabels := asMap(asMap(nsSel)["matchLabels"])
			if len(asMap(nsSel)) == 0 || len(nsLabels) == 0 || asMap(nsSel)["matchExpressions"] != nil {
				t.Errorf("[%s] ingress rule %d: namespaceSelector must pin kubernetes.io/metadata.name, got %v", overlay, i, nsSel)
				continue
			}
			ns, _ := nsLabels["kubernetes.io/metadata.name"].(string)
			wantPod, scoped := podScopedNamespaces[ns]
			if !scoped {
				continue
			}
			podLabels := asMap(asMap(peer["podSelector"])["matchLabels"])
			if got, _ := podLabels["app.kubernetes.io/name"].(string); got != wantPod || len(podLabels) == 0 {
				t.Errorf("[%s] ingress rule %d admits namespace %q without its pod selector (want app.kubernetes.io/name=%s, got %v)",
					overlay, i, ns, wantPod, peer["podSelector"])
				continue
			}
			ports := asSlice(rule["ports"])
			if len(ports) != 1 || toInt(asMap(ports[0])["port"]) != apiPort {
				t.Errorf("[%s] ingress rule %d: namespace %q must be admitted on the API port %d only, got %v", overlay, i, ns, apiPort, ports)
			}
			admitted[ns] = true
		}
	}
	for ns := range podScopedNamespaces {
		if !admitted[ns] {
			t.Errorf("[%s] namespace %q is not admitted at all (its caller would be blocked at L4)", overlay, ns)
		}
	}
}

// ── rendering + helpers ──────────────────────────────────────────────────

func render(t *testing.T, overlay string) []map[string]any {
	t.Helper()
	return renderArgs(t, overlay, "-f", filepath.Join(chartDir(), overlay))
}

func chartDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "deploy", "helm")
}

// renderArgs renders the chart with the base values plus extra helm args
// (overlays, --set) and returns the parsed documents. label names the
// combination in failure messages.
func renderArgs(t *testing.T, label string, extra ...string) []map[string]any {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("helm not on PATH (run .github/scripts/helm-check.sh once to install it): %v", err)
	}
	chart := chartDir()
	args := append([]string{"template", releaseName, chart, "--namespace", namespace}, sharedSets...)
	args = append(args, "-f", filepath.Join(chart, "values.yaml"))
	args = append(args, extra...)
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(t.Context(), "helm", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template [%s] failed: %v\n%s", label, err, stderr.String())
	}
	var docs []map[string]any
	dec := yaml.NewDecoder(&stdout)
	for {
		var d map[string]any
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse rendered YAML [%s]: %v", label, err)
		}
		if d != nil {
			docs = append(docs, d)
		}
	}
	return docs
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func strs(v any) []string {
	var out []string
	for _, e := range asSlice(v) {
		out = append(out, fmt.Sprint(e))
	}
	return out
}

func sorted(s []string) []string {
	sort.Strings(s)
	return s
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return -1
}
