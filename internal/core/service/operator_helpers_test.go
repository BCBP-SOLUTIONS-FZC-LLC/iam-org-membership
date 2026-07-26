package service

import (
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/stretchr/testify/assert"
)

// ── indexOf ───────────────────────────────────────────────────────────

func TestIndexOf_FindsSubstring(t *testing.T) {
	assert.Equal(t, 0, indexOf("hello", "hel"))
	assert.Equal(t, 6, indexOf("hello world", "world"))
	assert.Equal(t, 2, indexOf("abcdef", "cd"))
}

func TestIndexOf_MissingReturnsNegative(t *testing.T) {
	assert.Equal(t, -1, indexOf("hello", "xyz"))
	assert.Equal(t, -1, indexOf("", "x"))
}

func TestIndexOf_EmptyNeedleReturnsZero(t *testing.T) {
	// Convention matches strings.Index: empty needle sits at position 0.
	assert.Equal(t, 0, indexOf("hello", ""))
	assert.Equal(t, 0, indexOf("", ""))
}

// ── contains ──────────────────────────────────────────────────────────

func TestContains_TrueWhenSubstringPresent(t *testing.T) {
	assert.True(t, contains("chk_system_department_active violated", "chk_system_department_active"))
	assert.True(t, contains("abc", "abc"))
	assert.True(t, contains("abc", ""))
}

func TestContains_FalseWhenAbsent(t *testing.T) {
	assert.False(t, contains("abc", "xyz"))
	assert.False(t, contains("", "x"))
	assert.False(t, contains("short", "longer than haystack"))
}

// ── isCheckViolation ──────────────────────────────────────────────────

func TestIsCheckViolation_NilErrorIsFalse(t *testing.T) {
	assert.False(t, isCheckViolation(nil, "chk_anything"))
}

func TestIsCheckViolation_MatchesConstraintNameInMessage(t *testing.T) {
	err := errors.New("ERROR: new row for relation \"departments\" violates check constraint \"chk_system_department_active\"")
	assert.True(t, isCheckViolation(err, "chk_system_department_active"))
}

func TestIsCheckViolation_UnrelatedErrorIsFalse(t *testing.T) {
	err := errors.New("unique_violation on some other index")
	assert.False(t, isCheckViolation(err, "chk_system_department_active"))
}

func TestIsCheckViolation_EmptyMessageIsFalse(t *testing.T) {
	// A wrapped error whose Error() is empty must not match — otherwise
	// contains("", "chk_x") would spuriously succeed via empty-substring
	// convention. Guard the fail-safe path.
	err := emptyMsgErr{}
	assert.False(t, isCheckViolation(err, "chk_anything"))
}

type emptyMsgErr struct{}

func (emptyMsgErr) Error() string { return "" }

// ── DeleteDepartmentBlocked (OP-3) ────────────────────────────────────

func TestOperator_DeleteDepartmentBlocked_ReturnsValidationError(t *testing.T) {
	s := &OperatorService{}
	err := s.DeleteDepartmentBlocked()

	var de *domain.DomainError
	assert.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
	assert.Equal(t, "cannot_delete_system_department", de.Details["code"],
		"OP-3: delete is permanently blocked; retire via is_active=false instead")
}
