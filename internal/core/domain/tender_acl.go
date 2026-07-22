package domain

import (
	"time"

	"github.com/google/uuid"
)

// TenderACLLevel mirrors the tender_acl_level enum (§4.1, §16 A17/A32(c)).
// HLD-aligned: view / edit / approve (was read/write/admin in an earlier rev).
type TenderACLLevel string

const (
	ACLView    TenderACLLevel = "view"
	ACLEdit    TenderACLLevel = "edit"
	ACLApprove TenderACLLevel = "approve"
)

// TenderACLEntry is an additive per-user overlay ACL for a specific tender.
// tender_id has no FK (cross-service: Tender Service owns tenders).
// tenant_membership_id composite-FKs (§16 A16, TAE-8).
//
// An entry is "active" iff deleted_at IS NULL AND (expires_at IS NULL OR
// expires_at > now()) — passive expiry via TAE-3, checked at read time.
type TenderACLEntry struct {
	ID                 uuid.UUID
	TenantID           uuid.UUID
	TenderID           uuid.UUID
	UserID             uuid.UUID
	TenantMembershipID uuid.UUID // composite FK anchor
	AccessLevel        TenderACLLevel
	GrantedBy          uuid.UUID  // audit (§16 A27, TAE-6)
	Reason             string     // audit-only, 500-char cap service-side
	ExpiresAt          *time.Time // passive expiry (TAE-7)
	RecordVersion      int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
	DeletedAt          *time.Time
}

// IsActive returns true when the entry is not soft-deleted and not expired.
func (e *TenderACLEntry) IsActive(now time.Time) bool {
	if e.DeletedAt != nil {
		return false
	}
	if e.ExpiresAt != nil && !e.ExpiresAt.After(now) {
		return false
	}
	return true
}
