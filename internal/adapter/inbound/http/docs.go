package http

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

// asyncapiYAML is the AsyncAPI 3.0 event contract, embedded at build time.
// Consumed by AsyncAPIHandler (asyncapi.go) which renders the custom event
// catalog page at /asyncapi.
//
// The OpenAPI spec is NOT embedded here — it is generated from // @…
// annotations on each handler by `make swag` (mirrors the sibling
// iam-user-profile2 pattern). The generated docs/swagger/docs.go registers
// the spec via swag.Register at init time; ginSwagger serves it at
// /swagger/*any (including /swagger/doc.json).
//
//go:embed asyncapi.yaml
var asyncapiYAML []byte

// AsyncAPIYAMLHandler serves the raw AsyncAPI 3.0 spec.
func AsyncAPIYAMLHandler(c *gin.Context) {
	c.Data(http.StatusOK, "application/yaml; charset=utf-8", asyncapiYAML)
}
