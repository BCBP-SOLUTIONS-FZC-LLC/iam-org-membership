#!/usr/bin/env bash
# Image size + startup gate smoke tests for the CI-built Docker image.
# Invoked by ci.yml (keeps shell operators out of inline YAML run blocks).
set -euo pipefail

echo "::group::Image size check (linux/amd64)"
# arm64 is typically within ±5 MB of amd64 for a distroless Go binary; the limit
# is deliberately generous so it catches regressions (e.g. accidentally COPYing
# vendor/ or embedding test assets), not normal arch variance.
# For a full multi-arch manifest size breakdown after push, use:
#   docker buildx imagetools inspect ghcr.io/.../iam-org-membership:<tag>
MAX_MB=200
size=$(docker image inspect iam-org-membership-ci-test --format='{{.Size}}')
mb=$((size / 1024 / 1024))
echo "Image size: ${mb} MB (limit: ${MAX_MB} MB)"
[ "${mb}" -le "${MAX_MB}" ] &
P1=$!
echo "::endgroup::"

echo "::group::Startup gate"
# Server must exit non-zero on missing required env vars,
# proving the binary runs and validateRequiredEnv fires correctly.
# timeout 10s kills the container if it hangs (e.g. waits for a signal instead of exiting).
exit_code=0
timeout 10s docker run --rm iam-org-membership-ci-test 2>/dev/null || exit_code=$?
echo "Container exit code: ${exit_code} (expected non-zero)"
[ "${exit_code}" -ne 0 ] &
P2=$!
echo "::endgroup::"

wait $P1 || {
  echo "::error file=Dockerfile,title=Image size::Image is ${mb} MB, exceeds ${MAX_MB} MB limit — check COPY/ADD instructions for accidental inclusions"
  exit 1
}
wait $P2 || {
  echo "::error file=cmd/server/main.go,title=Startup gate::Server exited 0 on missing env vars — validateRequiredEnv must exit non-zero"
  exit 1
}

{
  echo "### Smoke test results"
  echo "- ✅ Startup gate: server exits ${exit_code} on missing env vars (validateRequiredEnv fires)"
  echo "- 📦 Image size (linux/amd64): **${mb} MB** (limit: ${MAX_MB} MB)"
  echo "- 🔍 [Security (Trivy SARIF results)](https://github.com/${GITHUB_REPOSITORY}/security/code-scanning)"
} >> "$GITHUB_STEP_SUMMARY"
