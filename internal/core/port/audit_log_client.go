package port

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// AuditLogClient is the outbound HTTP adapter for iam-audit-log's AL-5
// (POST /api/v1/internal/audit-entries). Only the D-6 direct-write subset
// is used here: config.tenant_setting.changed, invitation.created,
// invitation.revoked, invitation.expired.
//
// All calls are best-effort and fail-open — a write failure is logged as a
// warning but never propagates to the caller. Audit-log outages must not
// degrade core IAM operations.
type AuditLogClient interface {
	// WriteEntry posts a single D-6 direct-write entry to AL-5.
	// Returns nil on 201 (created) or 200 (idempotent replay).
	// Any other response is returned as an error; callers discard it.
	WriteEntry(ctx context.Context, req AuditWriteRequest) error
}

// AuditWriteRequest is the AL-5 DirectWriteRequest wire shape for the
// D-6 entry types that iam-org-membership is authorised to write.
type AuditWriteRequest struct {
	// IdempotencyKey is sent as the Idempotency-Key header (1–256 chars).
	// Use a stable, deterministic value so AL-5's dedup ledger handles retries.
	IdempotencyKey string

	TenantID        uuid.UUID
	EntryType       string // D-6 allow-list entry type
	Action          string // "update", "create", "revoke", "expire"
	Actor           AuditActor
	Target          *AuditTarget // nil for tenant-wide actions
	OccurredAt      time.Time
	SourceService   string         // always "iam-org-membership"
	SourceEventType string         // producer's verbatim type label
	IPAddress       string         // optional
	UserAgent       string         // optional
	Metadata        map[string]any // optional; forwarded as JSON object, ≤ 8192 B
}

// AuditActor mirrors AL-5's actor wire object.
type AuditActor struct {
	Type    string // "user" | "iam_system" | "anonymous"
	ID      string // UUID string; "" when Type == "anonymous"
	Display string // optional display name
}

// AuditTarget mirrors AL-5's optional target wire object.
type AuditTarget struct {
	Type string // e.g. "tenant_setting", "invitation"
	ID   string // optional identifier
}
