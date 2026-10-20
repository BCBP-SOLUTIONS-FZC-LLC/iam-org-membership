package domain

// EligibilityReason says why an assignee is not eligible to hold a task at
// (department, required_level) — I-19, the Workflow Services' assignee
// eligibility read. Empty when Eligible is true. Fixed set: it doubles as
// the metric result label.
type EligibilityReason string

const (
	EligibilityNotMember           EligibilityReason = "not_member"
	EligibilityMemberNotActive     EligibilityReason = "member_not_active"
	EligibilityDepartmentNotActive EligibilityReason = "department_not_active"
	EligibilityNotInDepartment     EligibilityReason = "not_in_department"
	EligibilityLevelInsufficient   EligibilityReason = "level_insufficient"
)

// AssigneeEligibility is the I-19 answer. RoleLevel is the user's current
// level in the department when they hold one (set for level_insufficient
// and for an eligible answer), otherwise empty.
type AssigneeEligibility struct {
	Eligible  bool
	Reason    EligibilityReason
	RoleLevel DeptRole
}
