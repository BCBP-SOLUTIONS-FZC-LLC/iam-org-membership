// validating_codec_fs_test.go — covers the error branches in
// newValidatingCodecFromFS that are unreachable via the embedded FS:
//
//   - ReadDir error (schemas dir missing) → soft-fail, returns codec + nil error
//   - Non-.json file → continue (skip) branch
//   - Invalid JSON file → json.Unmarshal error → returns error
//   - Valid JSON but not a valid jsonschema → compiler.Compile error → returns error
//   - ReadFile error → returns error with file name in message
//
// Uses testing/fstest.MapFS (stdlib) or custom fs.FS to inject a controlled FS.
package eventbus

import (
	"errors"
	"io"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewValidatingCodecFromFS_ReadDirError_SoftFail verifies that when the
// "schemas" directory does not exist in the injected FS (triggering a ReadDir
// error), newValidatingCodecFromFS returns a valid codec and a nil error —
// the deliberate soft-fail so the service boots without Phase 3 schemas.
func TestNewValidatingCodecFromFS_ReadDirError_SoftFail(t *testing.T) {
	// An empty MapFS has no "schemas" dir → ReadDir("schemas") returns error.
	emptyFS := fstest.MapFS{}

	c, err := newValidatingCodecFromFS(NoopCodec{}, emptyFS)

	require.NoError(t, err, "ReadDir error must be swallowed (intentional soft-fail)")
	require.NotNil(t, c, "codec must be non-nil even when ReadDir fails")
	assert.Empty(t, c.schemas, "no schemas must be loaded when ReadDir fails")
}

// TestNewValidatingCodecFromFS_NonJSONFile_Skipped verifies that a non-.json
// file in the schemas/ directory is skipped (the HasSuffix continue branch).
func TestNewValidatingCodecFromFS_NonJSONFile_Skipped(t *testing.T) {
	// Only a .txt file — no .json files — so the loop body's continue executes
	// but no schemas are loaded.
	mapFS := fstest.MapFS{
		"schemas/README.txt": {Data: []byte("not a schema")},
	}

	c, err := newValidatingCodecFromFS(NoopCodec{}, mapFS)

	require.NoError(t, err, "non-.json files must be silently skipped")
	require.NotNil(t, c)
	assert.Empty(t, c.schemas, "no schemas must be loaded for non-.json files")
}

// TestNewValidatingCodecFromFS_InvalidJSON_ReturnsError verifies that a file
// with an invalid JSON payload causes newValidatingCodecFromFS to return an
// error wrapping the json.Unmarshal failure.
func TestNewValidatingCodecFromFS_InvalidJSON_ReturnsError(t *testing.T) {
	mapFS := fstest.MapFS{
		"schemas/BadEvent.json": {Data: []byte(`not valid json {{{`)},
	}

	_, err := newValidatingCodecFromFS(NoopCodec{}, mapFS)

	require.Error(t, err, "invalid JSON must cause an error")
	assert.Contains(t, err.Error(), "parse schema BadEvent.json",
		"error must identify the offending schema file")
}

// TestNewValidatingCodecFromFS_ValidJSONButBadSchema_ReturnsError verifies
// that a valid JSON document that cannot be compiled as a JSON Schema causes
// newValidatingCodecFromFS to return an error wrapping the compile failure.
// We use a schema that references an undefined $ref, which the compiler
// cannot resolve.
func TestNewValidatingCodecFromFS_ValidJSONButBadSchema_ReturnsError(t *testing.T) {
	// A JSON object that parses fine but is not a valid JSON Schema —
	// referencing an undefined $ref causes the compiler to fail.
	badSchema := `{"$schema":"https://json-schema.org/draft-07/schema#","$ref":"#/definitions/NonExistent","definitions":{}}`
	mapFS := fstest.MapFS{
		"schemas/BrokenRef.json": {Data: []byte(badSchema)},
	}

	_, err := newValidatingCodecFromFS(NoopCodec{}, mapFS)

	require.Error(t, err, "unresolvable $ref must cause a compile error")
	// The error will either be a "compile schema" or "register schema" error.
	assert.True(t,
		contains(err.Error(), "compile schema BrokenRef.json") ||
			contains(err.Error(), "register schema BrokenRef.json"),
		"error must identify the offending schema file, got: %s", err.Error())
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// readErrorFS is a custom fs.FS whose ReadDir returns a file that then
// fails to ReadFile. This triggers the `fs.ReadFile` error branch (line 56-58).
type readErrorFS struct {
	listing []fakeFileEntry
}

type fakeFileEntry struct {
	name string
}

func (e fakeFileEntry) Name() string      { return e.name }
func (e fakeFileEntry) IsDir() bool       { return false }
func (e fakeFileEntry) Type() fs.FileMode { return 0 }
func (e fakeFileEntry) Info() (fs.FileInfo, error) {
	return nil, errors.New("no info")
}

func (f *readErrorFS) Open(name string) (fs.File, error) {
	// This is called by fs.ReadFile and fs.ReadDir internally.
	if name == "." || name == "schemas" {
		return &fakeDir{dirEntries: f.listing}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: errors.New("cannot read file")}
}

type fakeDir struct {
	dirEntries []fakeFileEntry
	read       bool
}

func (d *fakeDir) Read(_ []byte) (int, error) { return 0, io.EOF }
func (d *fakeDir) Close() error               { return nil }
func (d *fakeDir) Stat() (fs.FileInfo, error) {
	return &fakeDirInfo{}, nil
}
func (d *fakeDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.read {
		return nil, io.EOF
	}
	d.read = true
	out := make([]fs.DirEntry, len(d.dirEntries))
	for i, e := range d.dirEntries {
		out[i] = e
	}
	return out, nil
}

type fakeDirInfo struct{}

func (i *fakeDirInfo) Name() string       { return "schemas" }
func (i *fakeDirInfo) Size() int64        { return 0 }
func (i *fakeDirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o755 }
func (i *fakeDirInfo) ModTime() time.Time { return time.Time{} }
func (i *fakeDirInfo) IsDir() bool        { return true }
func (i *fakeDirInfo) Sys() any           { return nil }

// TestNewValidatingCodecFromFS_ReadFileError_ReturnsError verifies that when
// ReadDir succeeds but ReadFile fails for a .json file, the codec returns an error.
func TestNewValidatingCodecFromFS_ReadFileError_ReturnsError(t *testing.T) {
	rErrFS := &readErrorFS{
		listing: []fakeFileEntry{{name: "BadReadable.json"}},
	}

	_, err := newValidatingCodecFromFS(NoopCodec{}, rErrFS)

	require.Error(t, err, "ReadFile error must cause newValidatingCodecFromFS to return an error")
	assert.Contains(t, err.Error(), "read schema BadReadable.json",
		"error must identify the file that failed to read")
}

// TestNewValidatingCodecFromFS_AddResourceError_ReturnsError covers the
// compiler.AddResource error branch (line 71): when the schema name contains
// a control character, the URL parser rejects it and AddResource returns an
// error. A file named "\x00bad.json" produces name="\x00bad" after TrimSuffix;
// AddResource("\x00bad", doc) returns a URL-parse error because null bytes are
// invalid in URLs (net/url: invalid control character in URL).
func TestNewValidatingCodecFromFS_AddResourceError_ReturnsError(t *testing.T) {
	// Use a readErrorFS variant that lists a file whose name contains a null
	// byte. fstest.MapFS normalises paths and rejects null bytes, so we need
	// the custom readErrorFS/fakeDir infrastructure to return such an entry.
	rErrFS := &nullByteNameFS{}

	_, err := newValidatingCodecFromFS(NoopCodec{}, rErrFS)

	require.Error(t, err, "invalid schema URL must cause AddResource to fail")
	assert.True(t,
		containsStr(err.Error(), "register schema") || containsStr(err.Error(), "parse"),
		"error must reference the schema registration failure, got: %s", err.Error())
}

// nullByteNameFS is an fs.FS whose "schemas" directory lists one .json file
// whose name contains a null byte. A null byte in a URL is rejected by
// net/url, causing jsonschema.Compiler.AddResource to return an error.
type nullByteNameFS struct{}

func (f *nullByteNameFS) Open(name string) (fs.File, error) {
	if name == "." || name == "schemas" {
		return &nullByteDir{}, nil
	}
	// ReadFile for the null-byte file — return valid JSON so we reach AddResource.
	return &nullByteFile{}, nil
}

type nullByteDir struct{ read bool }

func (d *nullByteDir) Read(_ []byte) (int, error) { return 0, io.EOF }
func (d *nullByteDir) Close() error               { return nil }
func (d *nullByteDir) Stat() (fs.FileInfo, error) { return &fakeDirInfo{}, nil }
func (d *nullByteDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.read {
		return nil, io.EOF
	}
	d.read = true
	// File name with null byte — valid Go string, invalid URL.
	return []fs.DirEntry{fakeFileEntry{name: "\x00invalid.json"}}, nil
}

type nullByteFile struct{ read bool }

func (f *nullByteFile) Read(buf []byte) (int, error) {
	if f.read {
		return 0, io.EOF
	}
	f.read = true
	data := []byte(`{"type":"object"}`)
	n := copy(buf, data)
	return n, nil
}
func (f *nullByteFile) Close() error               { return nil }
func (f *nullByteFile) Stat() (fs.FileInfo, error) { return &fakeDirInfo{}, nil }
