#!/usr/bin/env bash
# Enforces the IAM Platform Observability Standard's naming rules against
# internal/adapter/outbound/metrics/business.go — the sole file that
# registers Prometheus collectors in this service.
#
# Checks, per registered collector:
#   - name matches ^(platform|iam)_[a-z0-9_]+$ (namespace + snake_case)
#   - every Counter/CounterVec name ends in _total
#   - every Histogram/HistogramVec name ends in _seconds
#   - no platform_* collector is hand-built with prometheus.New*: Tier 1
#     collectors must come from gincommon.NewShared*Vec (registry-owned name,
#     help, labels and buckets — shared with platform-events), in
#     registerShared()
#   - an iam_* collector's ConstLabels is serviceLabels(...) (gincommon's
#     {domain, service, environment} identity — Tier 2 and Tier 3 alike)
#
# Does NOT (and structurally cannot) validate the classification judgment
# itself (shared vs. service-specific) — that's a review-time call per the
# standard's decision tree, not a lint rule.
set -euo pipefail

FILE="internal/adapter/outbound/metrics/business.go"
test -f "$FILE" || {
  echo "::error file=${FILE}::metrics source file not found"
  exit 1
}

python3 - "$FILE" <<'PYEOF'
import re
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as f:
    text = f.read()

# Only scan registerMetrics()'s body — Register()/other functions don't
# construct collectors.
m = re.search(r"func registerMetrics\(.*?\n\}\n", text, re.DOTALL)
if not m:
    print(f"::error file={path}::could not locate registerMetrics() body")
    sys.exit(1)
body = m.group(0)

# One block per collector construction: kind (Counter/CounterVec/
# Histogram/HistogramVec/Gauge/GaugeVec), Name literal, ConstLabels var.
# The Opts{...} struct literal is captured up to its OWN closing brace
# (a line holding just a single tab + "}") rather than the first bare "}"
# encountered, since a HistogramOpts' Buckets: []float64{...} field has an
# inline closing brace of its own that would otherwise truncate the match
# early and hide a later ConstLabels: line.
block_re = re.compile(
    r"prometheus\.New(?P<kind>CounterVec|Counter|HistogramVec|Histogram|GaugeVec|Gauge)\("
    r"prometheus\.\w+Opts\{"
    r"(?P<opts>.*?)"
    r"\n\t\}",
    re.DOTALL,
)

name_re = re.compile(r'Name:\s*"([^"]+)"')
constlabels_re = re.compile(r"ConstLabels:\s*(\w+)")
namespace_re = re.compile(r"^(platform|iam)_[a-z0-9_]+$")

errors = []
count = 0
for block in block_re.finditer(body):
    kind = block.group("kind")
    opts = block.group("opts")
    name_m = name_re.search(opts)
    if not name_m:
        errors.append(f"a {kind} collector has no Name literal in its Opts (line ~{body[:block.start()].count(chr(10))+1})")
        continue
    name = name_m.group(1)
    count += 1

    if not namespace_re.match(name):
        errors.append(f'"{name}": must match ^(platform|iam)_[a-z0-9_]+$ (namespace + snake_case)')

    if kind in ("Counter", "CounterVec") and not name.endswith("_total"):
        errors.append(f'"{name}": {kind} must end in _total')
    if kind in ("Histogram", "HistogramVec") and not name.endswith("_seconds"):
        errors.append(f'"{name}": {kind} must end in _seconds')

    cl_m = constlabels_re.search(opts)
    const_labels = cl_m.group(1) if cl_m else None
    is_platform = name.startswith("platform_")
    is_iam = name.startswith("iam_")
    if is_platform:
        errors.append(f'"{name}": platform_* collectors must be built from the Platform Observability Registry with gincommon.NewShared*Vec (registerShared), not prometheus.New*')
    if is_iam and const_labels != "sLabels":
        errors.append(f'"{name}": iam_* collector must use ConstLabels: sLabels (gincommon identity: domain+service+environment), got {const_labels!r}')

# Tier 1: registerShared() may only build registry names via NewShared*.
shared = re.search(r"func registerShared\(\).*?\n\}\n", text, re.DOTALL)
if shared:
    for nm in re.findall(r'gincommon\.NewShared\w+\(nil, "([^"]+)"\)', shared.group(0)):
        count += 1
        if not nm.startswith("platform_"):
            errors.append(f'"{nm}": registerShared() is for registry platform_* metrics only')
for raw in re.findall(r'Name:\s*"(platform_[a-z0-9_]+)"', text):
    errors.append(f'"{raw}": hand-built platform_* collector — use gincommon.NewShared*Vec')

if errors:
    for e in errors:
        print(f"::error file={path}::{e}")
    print(f"\n{len(errors)} naming-convention violation(s) across {count} registered collectors.")
    sys.exit(1)

print(f"  ✔  {count} registered collectors — naming, suffix, and label-tier rules OK")
PYEOF
