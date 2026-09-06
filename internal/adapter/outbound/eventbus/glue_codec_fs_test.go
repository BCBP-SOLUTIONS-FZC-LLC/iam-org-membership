// glue_codec_fs_test.go — covers the error branches in allSchemaNamesFromFS
// that are unreachable via the embedded FS:
//
//   - ReadDir error → returns nil, error
//   - Non-.json file → continue (skip) branch
//
// Uses testing/fstest.MapFS (stdlib) to inject a controlled FS.
package eventbus

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAllSchemaNamesFromFS_ReadDirError_ReturnsError verifies that when the
// "schemas" directory does not exist in the injected FS (triggering a ReadDir
// error), allSchemaNamesFromFS propagates the error rather than silently
// returning an empty slice.
func TestAllSchemaNamesFromFS_ReadDirError_ReturnsError(t *testing.T) {
	// Empty MapFS has no "schemas" dir → ReadDir("schemas") returns an error.
	emptyFS := fstest.MapFS{}

	names, err := allSchemaNamesFromFS(emptyFS)

	require.Error(t, err, "ReadDir error must be propagated by allSchemaNamesFromFS")
	assert.Nil(t, names, "nil names must be returned on error")
}

// TestAllSchemaNamesFromFS_NonJSONFile_Skipped verifies that a non-.json
// file in the schemas/ directory is skipped (the HasSuffix continue branch)
// and only .json files contribute to the returned names.
func TestAllSchemaNamesFromFS_NonJSONFile_Skipped(t *testing.T) {
	mapFS := fstest.MapFS{
		"schemas/README.txt":   {Data: []byte("not a schema")},
		"schemas/MyEvent.json": {Data: []byte(`{"type":"object"}`)},
	}

	names, err := allSchemaNamesFromFS(mapFS)

	require.NoError(t, err)
	require.Len(t, names, 1, "only the .json file should be counted")
	assert.Equal(t, "MyEvent", names[0], "name must have .json extension stripped")
}

func TestAllSchemaNamesFromFS_UsesTitleNotFilename(t *testing.T) {
	mapFS := fstest.MapFS{
		"schemas/mfareset.json": {Data: []byte(`{"title":"MFAResetPayload","type":"object"}`)},
	}

	names, err := allSchemaNamesFromFS(mapFS)

	require.NoError(t, err)
	require.Equal(t, []string{"MFAReset"}, names,
		"Glue name must come from title, not the snake_case extract stem")
}

// TestAllSchemaNamesFromFS_EmptyDir_ReturnsEmpty verifies that a schemas/
// directory with no .json files returns an empty (not nil) slice without error.
func TestAllSchemaNamesFromFS_EmptyDir_ReturnsEmpty(t *testing.T) {
	// schemas/.keep has no .json suffix — should be skipped, leaving empty result.
	mapFS := fstest.MapFS{
		"schemas/.keep": {Data: []byte{}},
	}

	names, err := allSchemaNamesFromFS(mapFS)

	require.NoError(t, err)
	assert.Empty(t, names, "no .json files → empty name list")
}
