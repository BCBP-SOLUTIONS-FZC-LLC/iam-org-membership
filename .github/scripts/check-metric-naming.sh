#!/usr/bin/env bash
# Enforces the Enterprise Platform Observability Standard's construction and
# naming rules against internal/adapter/outbound/metrics/ — the sole package
# that builds Prometheus collectors in this service. (platform-gincommon's
# metricslint, .github/scripts/metricslint.sh, validates the emitted scrape;
# this check pins how the collectors are built.)
#
# Rules:
#   - no collector is built with prometheus.New* in non-test code: every one
#     comes from a gincommon constructor, which injects the {domain, service,
#     environment} const labels and validates the name
#   - registerShared() builds only registry names (platform_* or
#     domain-shared iam_*) with gincommon.NewShared*Vec
#   - registerServiceMetrics() builds only Tier 3 iam_org_membership_* names
#     with gincommon.NewCounterVec / NewCounter / NewGaugeVec / NewGauge /
#     NewHistogramVec: snake_case, _total on counters, _seconds on
#     histograms, no _total on gauges
#
# Does NOT (and structurally cannot) validate the classification judgment
# itself (shared vs. service-specific) — that's a review-time call per the
# standard's decision tree, not a lint rule.
set -euo pipefail

DIR="internal/adapter/outbound/metrics"
test -d "$DIR" || {
  echo "::error file=${DIR}::metrics package not found"
  exit 1
}

python3 - "$DIR" <<'PYEOF'
import glob
import re
import sys

pkg = sys.argv[1]
errors = []

sources = {p: open(p, encoding="utf-8").read() for p in sorted(glob.glob(f"{pkg}/*.go")) if not p.endswith("_test.go")}

for path, text in sources.items():
    for m in re.finditer(r"prometheus\.New(Counter|CounterVec|Gauge|GaugeVec|Histogram|HistogramVec|Summary|SummaryVec)\(", text):
        line = text[: m.start()].count("\n") + 1
        errors.append(f"{path}:{line}: prometheus.New{m.group(1)} — build collectors with gincommon's constructors (NewShared*Vec for registry metrics, NewCounterVec/NewGauge/NewHistogramVec for Tier 3)")

business = sources.get(f"{pkg}/business.go", "")

def body(name):
    m = re.search(rf"func {name}\(\)[^{{]*\{{\n(.*?)\n\}}\n", business, re.DOTALL)
    if not m:
        errors.append(f"{pkg}/business.go: could not locate {name}() body")
        return ""
    return m.group(1)

count = 0
for name in re.findall(r'gincommon\.NewShared\w+\(nil, "([^"]+)"\)', body("registerShared")):
    count += 1
    if not (name.startswith("platform_") or (name.startswith("iam_") and not name.startswith("iam_org_membership_"))):
        errors.append(f'"{name}": registerShared() builds registry metrics only (platform_* or domain-shared iam_*)')

tier3 = re.findall(r'gincommon\.New(CounterVec|Counter|GaugeVec|Gauge|HistogramVec)\(nil,\s*"([^"]+)"', body("registerServiceMetrics"))
if not tier3:
    errors.append(f"{pkg}/business.go: registerServiceMetrics() builds no collectors")
for kind, name in tier3:
    count += 1
    if not re.fullmatch(r"iam_org_membership_[a-z0-9_]+", name):
        errors.append(f'"{name}": Tier 3 collectors must be iam_org_membership_<snake_case>')
    if kind.startswith("Counter") and not name.endswith("_total"):
        errors.append(f'"{name}": {kind} must end in _total')
    if kind.startswith("Histogram") and not name.endswith("_seconds"):
        errors.append(f'"{name}": {kind} must end in _seconds')
    if kind.startswith("Gauge") and name.endswith("_total"):
        errors.append(f'"{name}": a gauge must not end in _total')

if errors:
    for e in errors:
        print(f"::error::{e}")
    print(f"\n{len(errors)} violation(s) across {count} collectors.")
    sys.exit(1)

print(f"  ✔  {count} collectors — all built with gincommon constructors; naming and suffix rules OK")
PYEOF
