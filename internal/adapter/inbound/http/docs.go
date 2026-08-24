package http

import (
	"net/http"

	"github.com/gin-gonic/gin"

	apispec "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/api"
)

// The OpenAPI spec is NOT embedded here — it is generated from // @…
// annotations on each handler by `make swag` (mirrors the sibling
// iam-user-profile2 pattern). The generated docs/swagger/docs.go registers
// the spec via swag.Register at init time; ginSwagger serves it at
// /swagger/*any (including /swagger/doc.json).
//
// The AsyncAPI 3.0 event contract itself is embedded directly from
// api/asyncapi.yaml via apispec.AsyncAPISpec (api/embed.go) — api/asyncapi.yaml
// is the single source of truth, with no synced duplicate copy to drift.
// Consumed by AsyncAPIHandler (asyncapi.go) which renders the custom event
// catalog page at /asyncapi.

// AsyncAPIYAMLHandler serves the raw AsyncAPI 3.0 spec.
func AsyncAPIYAMLHandler(c *gin.Context) {
	c.Data(http.StatusOK, "application/yaml; charset=utf-8", apispec.AsyncAPISpec)
}
