package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEnvBool(t *testing.T) {
	const key = "IAM_OM_TEST_ENV_BOOL"
	for _, tc := range []struct {
		val  string
		def  bool
		want bool
	}{
		{"", false, false},
		{"", true, true},
		{"true", false, true},
		{" TRUE ", false, true},
		{"1", false, true},
		{"false", true, false},
		{"0", true, false},
		{"not-a-bool", true, true},
		{"not-a-bool", false, false},
	} {
		t.Setenv(key, tc.val)
		assert.Equal(t, tc.want, envBool(key, tc.def), "value %q default %v", tc.val, tc.def)
	}
}
