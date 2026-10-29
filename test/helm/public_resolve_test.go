//go:build helm

// PUB-1 at the edge (LLD §5.4/§10.2, PUBRES-3; deploy/helm/templates/
// publicresolve.yaml). Pins the rendered Envoy Gateway resources that let an
// anonymous browser reach GET /api/v1/public/tenants/resolve:
//
//   - a dedicated HTTPRoute: one rule, path Exact, method GET, the main
//     route's hostnames (Gateway API precedence then prefers it over the
//     main route's PathPrefix /api/v1 for that exact GET, and only for it);
//   - a SecurityPolicy on that route only, with no mergeType and no
//     jwt/extAuth — it replaces the gateway's JWT + ext_authz there;
//   - a BackendTrafficPolicy on that route only with the per-client-IP
//     (sourceCIDR Distinct) global limit, 20/Hour by default;
//   - none of it by default, and the main route's SecurityPolicy unchanged
//     (still merging, so the gateway auth stays on everything else).
//
// Run: make helm-access-check (or go test -tags=helm ./test/helm/...).
package helm_test

import (
	"fmt"
	"testing"
)

const (
	publicResolvePath  = "/api/v1/public/tenants/resolve"
	publicResolveRoute = releaseName + "-public-resolve"
)

var publicResolveOn = []string{
	"--set", "ingress.enabled=true",
	"--set", "ingress.type=HTTPRoute",
	"--set", "ingress.gateway.name=platform-gw",
	"--set", "ingress.publicResolve.enabled=true",
	"--set", "ingress.securityPolicy.enabled=true",
	"--set", "ingress.securityPolicy.cors.enabled=true",
	"--set", "ingress.securityPolicy.cors.allowOrigins={https://app.bcbpsolutions.com}",
}

func findDoc(docs []map[string]any, kind, name string) map[string]any {
	for _, d := range docs {
		if d["kind"] == kind && asMap(d["metadata"])["name"] == name {
			return d
		}
	}
	return nil
}

func TestPublicResolve_OffByDefault(t *testing.T) {
	for _, overlay := range []string{"values-dev.yaml", "values-staging.yaml", "values-prod.yaml"} {
		docs := render(t, overlay)
		for _, kind := range []string{"HTTPRoute", "SecurityPolicy", "BackendTrafficPolicy"} {
			if findDoc(docs, kind, publicResolveRoute) != nil {
				t.Errorf("[%s] %s %s rendered although ingress.publicResolve is off", overlay, kind, publicResolveRoute)
			}
		}
	}
	// HTTPRoute mode alone does not open PUB-1 either.
	docs := renderArgs(t, "httproute", "--set", "ingress.enabled=true", "--set", "ingress.gateway.name=platform-gw")
	if findDoc(docs, "HTTPRoute", releaseName) == nil {
		t.Fatal("main HTTPRoute not rendered in HTTPRoute mode")
	}
	if findDoc(docs, "HTTPRoute", publicResolveRoute) != nil {
		t.Error("public route rendered without ingress.publicResolve.enabled")
	}
}

func TestPublicResolve_Route_ExactGETOnly(t *testing.T) {
	docs := renderArgs(t, "publicResolve", publicResolveOn...)
	route := findDoc(docs, "HTTPRoute", publicResolveRoute)
	if route == nil {
		t.Fatal("public HTTPRoute not rendered")
	}
	main := findDoc(docs, "HTTPRoute", releaseName)
	if main == nil {
		t.Fatal("main HTTPRoute not rendered")
	}
	spec := asMap(route["spec"])
	if got, want := fmt.Sprint(strs(spec["hostnames"])), fmt.Sprint(strs(asMap(main["spec"])["hostnames"])); got != want {
		t.Errorf("hostnames %s, want the main route's %s (precedence compares hostname first)", got, want)
	}
	if got := asMap(asSlice(spec["parentRefs"])[0])["name"]; got != "platform-gw" {
		t.Errorf("parentRef %v, want platform-gw", got)
	}
	rules := asSlice(spec["rules"])
	if len(rules) != 1 {
		t.Fatalf("want exactly 1 rule, got %d", len(rules))
	}
	matches := asSlice(asMap(rules[0])["matches"])
	if len(matches) != 1 {
		t.Fatalf("want exactly 1 match, got %d", len(matches))
	}
	m := asMap(matches[0])
	path := asMap(m["path"])
	if path["type"] != "Exact" || path["value"] != publicResolvePath {
		t.Errorf("path match %v, want Exact %s", path, publicResolvePath)
	}
	if m["method"] != "GET" {
		t.Errorf("method %v, want GET only", m["method"])
	}
	backend := asMap(asSlice(asMap(rules[0])["backendRefs"])[0])
	if backend["name"] != releaseName || toInt(backend["port"]) != apiPort {
		t.Errorf("backendRef %v, want %s:%d", backend, releaseName, apiPort)
	}
}

func TestPublicResolve_SecurityPolicy_NoMergeNoAuth(t *testing.T) {
	docs := renderArgs(t, "publicResolve", publicResolveOn...)
	sp := findDoc(docs, "SecurityPolicy", publicResolveRoute)
	if sp == nil {
		t.Fatal("public SecurityPolicy not rendered")
	}
	spec := asMap(sp["spec"])
	target := asMap(spec["targetRef"])
	if target["kind"] != "HTTPRoute" || target["name"] != publicResolveRoute {
		t.Errorf("targetRef %v, want HTTPRoute %s only", target, publicResolveRoute)
	}
	for _, k := range []string{"mergeType", "jwt", "extAuth", "oidc", "basicAuth", "apiKeyAuth", "authorization", "targetRefs", "targetSelectors"} {
		if _, ok := spec[k]; ok {
			t.Errorf("public SecurityPolicy must not set %s — it would re-apply or widen gateway auth", k)
		}
	}
	cors := asMap(spec["cors"])
	if fmt.Sprint(strs(cors["allowMethods"])) != "[GET]" {
		t.Errorf("cors.allowMethods %v, want [GET]", cors["allowMethods"])
	}

	// The main route's policy still merges with the gateway's JWT + ext_authz.
	mainSP := findDoc(docs, "SecurityPolicy", releaseName)
	if mainSP == nil {
		t.Fatal("main SecurityPolicy not rendered")
	}
	if asMap(mainSP["spec"])["mergeType"] != "StrategicMerge" {
		t.Errorf("main SecurityPolicy mergeType %v, want StrategicMerge", asMap(mainSP["spec"])["mergeType"])
	}
}

func TestPublicResolve_RateLimit_PerClientIP(t *testing.T) {
	docs := renderArgs(t, "publicResolve", publicResolveOn...)
	btp := findDoc(docs, "BackendTrafficPolicy", publicResolveRoute)
	if btp == nil {
		t.Fatal("public BackendTrafficPolicy not rendered")
	}
	spec := asMap(btp["spec"])
	target := asMap(spec["targetRef"])
	if target["kind"] != "HTTPRoute" || target["name"] != publicResolveRoute {
		t.Errorf("targetRef %v, want HTTPRoute %s only", target, publicResolveRoute)
	}
	rules := asSlice(asMap(asMap(spec["rateLimit"])["global"])["rules"])
	var cidrs []string
	for _, r := range rules {
		rule := asMap(r)
		sel := asSlice(rule["clientSelectors"])
		if len(sel) != 1 {
			t.Fatalf("rule %v: want exactly one client selector (selectors are ANDed)", rule)
		}
		src := asMap(asMap(sel[0])["sourceCIDR"])
		if src["type"] != "Distinct" {
			t.Errorf("sourceCIDR type %v, want Distinct (one bucket per client IP)", src["type"])
		}
		cidrs = append(cidrs, fmt.Sprint(src["value"]))
		limit := asMap(rule["limit"])
		if toInt(limit["requests"]) != 20 || limit["unit"] != "Hour" {
			t.Errorf("limit %v, want 20/Hour (PUBRES-3)", limit)
		}
	}
	if fmt.Sprint(sorted(cidrs)) != "[0.0.0.0/0 ::/0]" {
		t.Errorf("CIDRs %v, want every IPv4 and IPv6 client", cidrs)
	}
}

func TestPublicResolve_RateLimit_ValuesDriven(t *testing.T) {
	args := append(append([]string{}, publicResolveOn...),
		"--set", "ingress.publicResolve.rateLimit.requests=50",
		"--set", "ingress.publicResolve.rateLimit.unit=Minute")
	docs := renderArgs(t, "publicResolve-50/min", args...)
	btp := findDoc(docs, "BackendTrafficPolicy", publicResolveRoute)
	for _, r := range asSlice(asMap(asMap(asMap(btp["spec"])["rateLimit"])["global"])["rules"]) {
		limit := asMap(asMap(r)["limit"])
		if toInt(limit["requests"]) != 50 || limit["unit"] != "Minute" {
			t.Errorf("limit %v, want 50/Minute", limit)
		}
	}
}

// The main route's gateway rate limit (ingress.rateLimit) renders as a
// BackendTrafficPolicy, the only Envoy Gateway kind that carries rateLimit;
// the main SecurityPolicy never carries one (Envoy Gateway rejects it).
func TestMainRoute_RateLimit_OnBackendTrafficPolicy(t *testing.T) {
	args := append(append([]string{}, publicResolveOn...), "--set", "ingress.rateLimit.enabled=true")
	docs := renderArgs(t, "rateLimit", args...)
	if _, ok := asMap(findDoc(docs, "SecurityPolicy", releaseName)["spec"])["rateLimit"]; ok {
		t.Error("main SecurityPolicy must not carry rateLimit")
	}
	btp := findDoc(docs, "BackendTrafficPolicy", releaseName)
	if btp == nil {
		t.Fatal("main BackendTrafficPolicy not rendered with ingress.rateLimit.enabled")
	}
	spec := asMap(btp["spec"])
	if target := asMap(spec["targetRef"]); target["kind"] != "HTTPRoute" || target["name"] != releaseName {
		t.Errorf("targetRef %v, want HTTPRoute %s", target, releaseName)
	}
	if spec["mergeType"] != "StrategicMerge" {
		t.Errorf("mergeType %v, want StrategicMerge", spec["mergeType"])
	}
	rules := asSlice(asMap(asMap(spec["rateLimit"])["global"])["rules"])
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rules))
	}
	if limit := asMap(asMap(rules[0])["limit"]); toInt(limit["requests"]) != 1000 || limit["unit"] != "Second" {
		t.Errorf("limit %v, want 1000/Second", limit)
	}

	// Off by default.
	if findDoc(renderArgs(t, "publicResolve", publicResolveOn...), "BackendTrafficPolicy", releaseName) != nil {
		t.Error("main BackendTrafficPolicy rendered although ingress.rateLimit is off")
	}
}
