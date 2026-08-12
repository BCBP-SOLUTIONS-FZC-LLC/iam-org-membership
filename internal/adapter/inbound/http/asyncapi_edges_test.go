// Phase 19 — asyncapi renderer edge coverage:
//   - propType — array<T>, array<$ref>, format-suffixed types, plain string types
//   - typeHTML — same variants plus $ref → schema anchor
//   - snsEventType — PascalCase key vs snake_case fallback, missing bindings
//   - walkYAML — mapping node walk vs missing / non-mapping node
//   - UnmarshalYAML property-order preservation
package http

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// propType ────────────────────────────────────────────────────────────

func TestP19AsyncEdges_PropType_PlainType(t *testing.T) {
	assert.Equal(t, "string", propType(&asyncProp{Type: "string"}))
}

func TestP19AsyncEdges_PropType_WithFormatSuffix(t *testing.T) {
	assert.Equal(t, "string(uuid)", propType(&asyncProp{Type: "string", Format: "uuid"}))
}

func TestP19AsyncEdges_PropType_ArrayOfPrimitive(t *testing.T) {
	assert.Equal(t, "array<string>", propType(&asyncProp{Type: "array", Items: &asyncProp{Type: "string"}}))
}

func TestP19AsyncEdges_PropType_ArrayOfRef(t *testing.T) {
	got := propType(&asyncProp{Type: "array", Items: &asyncProp{Ref: "#/components/schemas/Payload"}})
	assert.Equal(t, "array<Payload>", got)
}

func TestP19AsyncEdges_PropType_RefDrivesResult(t *testing.T) {
	assert.Equal(t, "Payload", propType(&asyncProp{Ref: "#/components/schemas/Payload"}))
}

// typeHTML ────────────────────────────────────────────────────────────

func TestP19AsyncEdges_TypeHTML_Ref_HasSchemaAnchor(t *testing.T) {
	got := typeHTML(&asyncProp{Ref: "#/components/schemas/Payload"})
	assert.Contains(t, got, `href="#schema-Payload"`)
	assert.Contains(t, got, `Payload`)
}

func TestP19AsyncEdges_TypeHTML_ArrayOfRef_HasSchemaAnchor(t *testing.T) {
	got := typeHTML(&asyncProp{Type: "array", Items: &asyncProp{Ref: "#/components/schemas/Payload"}})
	assert.Contains(t, got, "array")
	assert.Contains(t, got, `href="#schema-Payload"`)
}

func TestP19AsyncEdges_TypeHTML_PlainString(t *testing.T) {
	got := typeHTML(&asyncProp{Type: "string", Format: "uuid"})
	assert.Contains(t, got, "string(uuid)")
}

// walkYAML ────────────────────────────────────────────────────────────

func TestP19AsyncEdges_WalkYAML_MissingKey_ReturnsEmpty(t *testing.T) {
	var root yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte("a: 1\nb: 2\n"), &root))
	assert.Equal(t, "", walkYAML(&root, "nonexistent"))
}

func TestP19AsyncEdges_WalkYAML_NestedMapping_ReturnsValue(t *testing.T) {
	var root yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte("outer:\n  inner: value42\n"), &root))
	assert.Equal(t, "value42", walkYAML(&root, "outer", "inner"))
}

func TestP19AsyncEdges_WalkYAML_NilNode_ReturnsEmpty(t *testing.T) {
	assert.Equal(t, "", walkYAML(nil, "x"))
}

func TestP19AsyncEdges_WalkYAML_EmptyKeys_ReturnsEmpty(t *testing.T) {
	var root yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte("a: b\n"), &root))
	assert.Equal(t, "", walkYAML(&root))
}

// snsEventType ────────────────────────────────────────────────────────

func TestP19AsyncEdges_SnsEventType_PascalCasePreferred(t *testing.T) {
	var root yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(`
sns:
  messageAttributes:
    EventType:
      value: TrialTenantProvisioned
`), &root))
	assert.Equal(t, "TrialTenantProvisioned", snsEventType(&root))
}

func TestP19AsyncEdges_SnsEventType_FallsBackToSnakeCase(t *testing.T) {
	var root yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(`
sns:
  messageAttributes:
    event_type:
      value: TenantConverted
`), &root))
	assert.Equal(t, "TenantConverted", snsEventType(&root))
}

func TestP19AsyncEdges_SnsEventType_NilBindings_ReturnsEmpty(t *testing.T) {
	assert.Equal(t, "", snsEventType(nil))
}

func TestP19AsyncEdges_SnsEventType_NoEventTypeKey_ReturnsEmpty(t *testing.T) {
	var root yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(`
sns:
  messageAttributes: {}
`), &root))
	assert.Equal(t, "", snsEventType(&root))
}

// UnmarshalYAML preserves property order ─────────────────────────────

func TestP19AsyncEdges_UnmarshalYAML_PreservesPropertyOrder(t *testing.T) {
	var s asyncSchema
	yamlText := `
type: object
properties:
  zebra:
    type: string
  alpha:
    type: integer
  middle:
    type: boolean
`
	require.NoError(t, yaml.Unmarshal([]byte(yamlText), &s))
	assert.Equal(t, []string{"zebra", "alpha", "middle"}, s.PropertyOrder, "declared order preserved")
	assert.Equal(t, 3, len(s.Properties))
}

func TestP19AsyncEdges_UnmarshalYAML_NoProperties_LeavesOrderEmpty(t *testing.T) {
	var s asyncSchema
	require.NoError(t, yaml.Unmarshal([]byte("type: object\n"), &s))
	assert.Empty(t, s.PropertyOrder)
}

// smoke — cover AsyncAPIHandler by calling it directly (loads embedded spec)
func TestP19AsyncEdges_AsyncAPIHandler_Renders200(t *testing.T) {
	c, w := buildCtx("GET", "/", ``, nil)
	AsyncAPIHandler(c)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	assert.True(t, strings.Contains(w.Body.String(), "<html"))
}
