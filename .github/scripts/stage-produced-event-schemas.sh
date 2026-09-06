#!/usr/bin/env bash
# Copy schema-gov extract output into DEST using PascalCase Glue names.
#
# extract 0.4 writes snake_case stems (mfareset.json, tenant_created.json).
# Glue and make schema-verify still use the payload title minus "Payload"
# (MFAReset, TenantCreated). Do not invent PascalCase from the filename.
#
# Usage: stage-produced-event-schemas.sh DEST [membership|tenant|all]
set -euo pipefail

DEST="${1:?destination directory required}"
LANE="${2:-all}"
SRC="${SCHEMA_SRC:-internal/adapter/outbound/eventbus/schemas}"

membership_names() {
  cat <<'EOF'
DepartmentMembershipGranted
DepartmentMembershipRevoked
DepartmentMembershipLevelChanged
TenantRoleGranted
TenantRoleRevoked
TenderAssigneeOverridden
MFAReset
TenantSeatOverageStarted
TenantSeatOverageResolved
TenantStateChanged
MembershipRevoked
TenantMembershipsPurged
EOF
}

tenant_names() {
  cat <<'EOF'
TenantCreated
TrialStarted
EOF
}

allowed_names() {
  case "$LANE" in
    membership) membership_names ;;
    tenant) tenant_names ;;
    all) membership_names; tenant_names ;;
    *)
      echo "unknown lane: $LANE (expected membership|tenant|all)" >&2
      exit 2
      ;;
  esac
}

mkdir -p "$DEST"
allow=$(allowed_names)

shopt -s nullglob
copied=0
for file in "$SRC"/*.json; do
  title=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("title",""))' "$file")
  name="${title%Payload}"
  [ -n "$name" ] || continue
  echo "$allow" | grep -qx "$name" || continue
  cp "$file" "$DEST/${name}.json"
  copied=$((copied + 1))
done

if [ "$copied" -eq 0 ]; then
  echo "no produced schemas staged into $DEST (lane=$LANE)" >&2
  exit 1
fi
echo "staged $copied produced schema(s) into $DEST (lane=$LANE)"
