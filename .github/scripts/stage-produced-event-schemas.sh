#!/usr/bin/env bash
# Copy this service's OWN produced-event schema files into DEST, renamed to
# their PascalCase Glue name.
#
# schema-gov extract 0.4 writes snake_case filenames under
# internal/adapter/outbound/eventbus/schemas/, and that one directory holds
# BOTH this service's produced schemas and consumed (other-service-owned)
# schemas kept only for schema-gov coverage (see glue_codec.go's
# domain.IsProducedEvent). schema-gov diff/register need the PascalCase Glue
# name and only the produced subset, so this hand-lists the
# filename→name mapping — the same convention as iam-delegation's inline
# SCHEMA_NAME_MAP in its schema-registry.yml. A `case` statement is used
# instead of a bash 4+ associative array so this also runs under macOS's
# default /bin/bash 3.2 (no Homebrew bash required) via `make
# schema-register`, not just under CI's ubuntu-latest. A new schema needs
# one line added to the matching case below.
#
# Usage: stage-produced-event-schemas.sh DEST [membership|tenant|all]
set -euo pipefail

DEST="${1:?destination directory required}"
LANE="${2:-all}"
SRC="${SCHEMA_SRC:-internal/adapter/outbound/eventbus/schemas}"

membership_name_for() {
  case "$1" in
    department_membership_granted)       echo DepartmentMembershipGranted ;;
    department_membership_revoked)       echo DepartmentMembershipRevoked ;;
    department_membership_level_changed) echo DepartmentMembershipLevelChanged ;;
    tenant_role_granted)                 echo TenantRoleGranted ;;
    tenant_role_revoked)                 echo TenantRoleRevoked ;;
    tender_assignee_overridden)          echo TenderAssigneeOverridden ;;
    mfareset)                            echo MFAReset ;;
    tenant_seat_overage_started)         echo TenantSeatOverageStarted ;;
    tenant_seat_overage_resolved)        echo TenantSeatOverageResolved ;;
    tenant_state_changed)                echo TenantStateChanged ;;
    membership_revoked)                  echo MembershipRevoked ;;
    tenant_memberships_purged)           echo TenantMembershipsPurged ;;
    *)                                   echo "" ;;
  esac
}

tenant_name_for() {
  case "$1" in
    tenant_created) echo TenantCreated ;;
    trial_started)  echo TrialStarted ;;
    *)              echo "" ;;
  esac
}

resolve_name() {
  local stem="$1"
  case "$LANE" in
    membership) membership_name_for "$stem" ;;
    tenant)     tenant_name_for "$stem" ;;
    all)
      local name
      name=$(membership_name_for "$stem")
      [ -n "$name" ] || name=$(tenant_name_for "$stem")
      echo "$name"
      ;;
    *)
      echo "unknown lane: $LANE (expected membership|tenant|all)" >&2
      exit 2
      ;;
  esac
}

mkdir -p "$DEST"

shopt -s nullglob
copied=0
for file in "$SRC"/*.json; do
  stem=$(basename "$file" .json)
  name=$(resolve_name "$stem")
  [ -n "$name" ] || continue
  cp "$file" "$DEST/${name}.json"
  copied=$((copied + 1))
done

if [ "$copied" -eq 0 ]; then
  echo "no produced schemas staged into $DEST (lane=$LANE)" >&2
  exit 1
fi
echo "staged $copied produced schema(s) into $DEST (lane=$LANE)"
