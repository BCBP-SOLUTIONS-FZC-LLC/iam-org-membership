package auditrecorder

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audit"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/auditoutbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/audittest"
	auditconfig "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-audit/pkg/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
)

// Every entry type this service emits is in Audit Log's taxonomy (and
// allowed from this source service, for producer-restricted types).
func TestEntryTypes_KnownToAuditLog(t *testing.T) {
	audittest.RequireKnownTypes(t, port.AuditEntryTypes...)
	for _, et := range port.AuditEntryTypes {
		kt, ok := audittest.LookupType(et)
		require.True(t, ok, et)
		assert.True(t, kt.AllowedFrom(SourceService), "%s must be accepted from %s", et, SourceService)
	}
}

func newRecorder(t *testing.T) *Recorder {
	t.Helper()
	enq, err := auditoutbox.NewEnqueuer(auditoutbox.EnqueueConfig{SourceService: SourceService, Strict: true})
	require.NoError(t, err)
	return New(enq)
}

func TestRecord_OutsideTransaction(t *testing.T) {
	r := newRecorder(t)
	err := r.Record(context.Background(), port.AuditEntry{EntryType: port.AuditEntryInvitationCreated})
	assert.ErrorIs(t, err, ErrNoTransaction)
	err = r.RecordBatch(context.Background(), []port.AuditEntry{{EntryType: port.AuditEntryInvitationExpired}})
	assert.ErrorIs(t, err, ErrNoTransaction)
}

func TestRecordBatch_EmptyIsNoop(t *testing.T) {
	assert.NoError(t, newRecorder(t).RecordBatch(context.Background(), nil))
}

func TestToEntry_MapsEveryField(t *testing.T) {
	tenant, inv, user := uuid.New(), uuid.New(), uuid.New()
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	got, err := toEntry(port.AuditEntry{
		IdempotencyKey:  "invitation.revoked:x:y",
		TenantID:        tenant,
		EntryType:       port.AuditEntryInvitationRevoked,
		Action:          "revoke",
		Actor:           port.UserAuditActor(user),
		Target:          &port.AuditTarget{Type: "invitation", ID: inv.String()},
		OccurredAt:      at,
		SourceEventType: "InvitationRevoked",
		IPAddress:       "10.0.0.1",
		UserAgent:       "ua",
		Metadata:        map[string]any{"name": "Acme"},
	})
	require.NoError(t, err)
	assert.Equal(t, tenant, got.TenantID)
	assert.Equal(t, port.AuditEntryInvitationRevoked, got.EntryType)
	assert.Equal(t, "revoke", got.Action)
	assert.Equal(t, audit.UserActor(user), got.Actor)
	assert.Equal(t, &audit.Target{Type: "invitation", ID: inv.String()}, got.Target)
	assert.Equal(t, at, got.OccurredAt)
	assert.Equal(t, "InvitationRevoked", got.SourceEventType)
	assert.Equal(t, "10.0.0.1", got.IPAddress)
	assert.Equal(t, "ua", got.UserAgent)
	assert.JSONEq(t, `{"name":"Acme"}`, string(got.Metadata))
	// SourceService is the Enqueuer's (AUDIT_SOURCE_SERVICE).
	assert.Empty(t, got.SourceService)
	// The entry is valid as AL-5 sees it once the Enqueuer fills defaults.
	require.NoError(t, audit.Validate(audit.FillDefaults(context.Background(), got, SourceService), "invitation.revoked:x:y", audit.DefaultMaxMetadataBytes))
}

func TestToEntry_SystemActorAndNoTargetOrMetadata(t *testing.T) {
	got, err := toEntry(port.AuditEntry{Actor: port.SystemAuditActor()})
	require.NoError(t, err)
	assert.Equal(t, audit.SystemActor(), got.Actor, "iam_system must carry the reserved …00a1 id")
	assert.Nil(t, got.Target)
	assert.Nil(t, got.Metadata)
}

func TestToEntry_UnencodableMetadata(t *testing.T) {
	_, err := toEntry(port.AuditEntry{EntryType: "x", Metadata: map[string]any{"f": func() {}}})
	var jerr *json.UnsupportedTypeError
	assert.ErrorAs(t, err, &jerr)
}

func TestAuditActorFor(t *testing.T) {
	assert.Equal(t, port.SystemAuditActor(), port.AuditActorFor(uuid.Nil))
	u := uuid.New()
	assert.Equal(t, port.UserAuditActor(u), port.AuditActorFor(u))
}

func TestLoadConfig_DefaultsSourceService(t *testing.T) {
	t.Setenv(auditconfig.EnvSourceService, "")
	t.Setenv(auditconfig.EnvBaseURL, "")
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, SourceService, cfg.SourceService)
	assert.False(t, cfg.DeliveryEnabled())
	assert.Equal(t, audit.CallerDomainIAM, cfg.CallerDomain)
}

func TestLoadConfig_RejectsMalformedValues(t *testing.T) {
	t.Setenv(auditconfig.EnvTimeout, "not-a-duration")
	_, err := LoadConfig()
	assert.ErrorIs(t, err, audit.ErrConfig)
}
