package port

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// AuditRecorder writes Audit Log (AL-5) entries into the platform-audit
// outbox (pending_audit_entries) on the transaction carried by ctx, so an
// entry exists if and only if the audited change commits (PA-INV-3). The
// outbox runner in cmd/server delivers it to iam-audit-log afterwards, with
// retries; an Audit Log outage never fails or slows the business write
// (AUDIT-CALLER-1).
//
// Callers never pass a pgx.Tx: TxRunner.RunInTx stores the open transaction
// and the recorder on ctx, exactly as it does for EventPublisher. Record
// outside a transaction is an error.
//
// An entry that fails the library's validation is logged, counted
// (platform_audit_invalid_entries_total) and dropped, and Record returns
// nil, so a malformed audit entry never rolls back the business write. Only
// a database error on the transaction itself is returned.
type AuditRecorder interface {
	Record(ctx context.Context, e AuditEntry) error
	// RecordBatch writes several entries in one statement (one per expired
	// invitation in the invitation-expiry sweep).
	RecordBatch(ctx context.Context, entries []AuditEntry) error
}

// AuditEntry is one AL-5 entry. SourceService is not a field: the adapter
// fills it from AUDIT_SOURCE_SERVICE.
type AuditEntry struct {
	// IdempotencyKey identifies the audited change, never the attempt:
	// the same change always produces the same key (≤ 256 bytes).
	IdempotencyKey  string
	TenantID        uuid.UUID
	EntryType       string // Audit Log taxonomy type, e.g. invitation.created
	Action          string // create | update | revoke | expire
	Actor           AuditActor
	Target          *AuditTarget // nil for tenant-wide actions
	OccurredAt      time.Time    // from the change (row timestamp), never the enqueue time
	SourceEventType string
	IPAddress       string         // optional
	UserAgent       string         // optional
	Metadata        map[string]any // optional; ids and setting values only, never personal data
}

// AL-5 entry types this service emits (Audit Log taxonomy, security_3y).
const (
	AuditEntryTenantSettingChanged = "config.tenant_setting.changed" // P-2
	AuditEntryInvitationCreated    = "invitation.created"            // P-6
	AuditEntryInvitationRevoked    = "invitation.revoked"            // P-31
	AuditEntryInvitationExpired    = "invitation.expired"            // invitation-expiry CronJob
)

// AuditEntryTypes lists every type above; a test checks it against Audit
// Log's taxonomy (audittest.RequireKnownTypes).
var AuditEntryTypes = []string{
	AuditEntryTenantSettingChanged,
	AuditEntryInvitationCreated,
	AuditEntryInvitationRevoked,
	AuditEntryInvitationExpired,
}

// AuditActorType is the AL-5 actor type.
type AuditActorType string

// AL-5 actor types this service emits.
const (
	AuditActorUser      AuditActorType = "user"
	AuditActorIAMSystem AuditActorType = "iam_system"
)

// AuditActor mirrors AL-5's actor object. ID is a canonical UUID: the
// user's IdP subject, or IAMSystemAuditActorID for iam_system.
type AuditActor struct {
	Type AuditActorType
	ID   string
}

// IAMSystemAuditActorID is the only id Audit Log accepts for an iam_system
// actor (Audit Log LLD §10.3). It is not domain.SystemActorID, the event
// envelope's system actor.
var IAMSystemAuditActorID = uuid.MustParse("00000000-0000-0000-0000-0000000000a1")

// UserAuditActor is a user actor identified by their IdP subject.
func UserAuditActor(sub uuid.UUID) AuditActor {
	return AuditActor{Type: AuditActorUser, ID: sub.String()}
}

// SystemAuditActor is {iam_system, …00a1}: automated actions and request
// paths without a caller identity.
func SystemAuditActor() AuditActor {
	return AuditActor{Type: AuditActorIAMSystem, ID: IAMSystemAuditActorID.String()}
}

// AuditActorFor is UserAuditActor(sub), or SystemAuditActor() when sub is
// uuid.Nil.
func AuditActorFor(sub uuid.UUID) AuditActor {
	if sub == uuid.Nil {
		return SystemAuditActor()
	}
	return UserAuditActor(sub)
}

// AuditTarget mirrors AL-5's optional target object.
type AuditTarget struct {
	Type string // e.g. "invitation"
	ID   string
}

type contextAuditRecorderKey struct{}

// AuditRecorderFromContext retrieves the recorder injected by
// TxRunner.RunInTx.
func AuditRecorderFromContext(ctx context.Context) (AuditRecorder, bool) {
	r, ok := ctx.Value(contextAuditRecorderKey{}).(AuditRecorder)
	return r, ok
}

// WithAuditRecorder stores a recorder in ctx. Production TxRunner injects
// the platform-audit adapter after attaching the running tx; tests inject
// fakes.
func WithAuditRecorder(ctx context.Context, r AuditRecorder) context.Context {
	return context.WithValue(ctx, contextAuditRecorderKey{}, r)
}
