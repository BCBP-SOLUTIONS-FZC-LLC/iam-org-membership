package http

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
)

// maxIdentifierLen caps PUB-1's identifier at RFC 5321's 254-character
// email length, which also covers every tenant slug.
const maxIdentifierLen = 254

// TenantResolver is the slice of service.TenantResolutionService PublicHandler
// needs; an interface so the handler is unit-testable with a fake.
type TenantResolver interface {
	Resolve(ctx context.Context, identifier string) (domain.TenantResolution, error)
}

// PublicHandler serves /api/v1/public/* — the one route class in this
// service with no authentication and no gateway-injected headers
// (PUBRES-1). Registered directly on the engine by registerPublicRoutes,
// never under the protected /api/v1 group.
type PublicHandler struct {
	resolver TenantResolver
}

// NewPublicHandler builds a PublicHandler.
func NewPublicHandler(resolver TenantResolver) *PublicHandler {
	return &PublicHandler{resolver: resolver}
}

// ResolveTenant is PUB-1 (LLD §5.4/§8.11, IAM HLD §8.2.7).
//
// @Summary      Pre-login tenant/realm resolution (PUB-1)
// @Description  Given a company slug or the email of a still-pending invitation, returns which Keycloak realm the frontend should redirect to. Reachable by an anonymous, pre-authentication browser: no auth, no gateway-injected headers. Always 200 for a well-formed identifier, never 404 (constant-shape anti-enumeration, PUBRES-2). tenant_id is present only for a shared-realm (trial) tenant (PUBRES-4). An already-active member's email is not resolvable (LLD §16 OQ-11).
// @Tags         public
// @Produce      json
// @Param        identifier  query     string  true  "Company slug or pending-invitation email (max 254 chars)"
// @Success      200         {object}  PublicTenantResolveResponse
// @Failure      400         {object}  ErrorResponse  "invalid_identifier"
// @Failure      503         {object}  ErrorResponse  "db_unavailable"
// @Router       /public/tenants/resolve [get]
func (h *PublicHandler) ResolveTenant(c *gin.Context) {
	identifier := strings.TrimSpace(c.Query("identifier"))
	if identifier == "" || len(identifier) > maxIdentifierLen {
		HandleError(c, domain.NewError(domain.ErrValidation, "identifier is required (max 254 characters)").
			WithDetails(map[string]any{"code": "invalid_identifier"}))
		return
	}
	res, err := h.resolver.Resolve(c.Request.Context(), identifier)
	if err != nil {
		HandleError(c, err)
		return
	}
	if !res.Found {
		metrics.IncPublicTenantResolve("not_found")
		c.JSON(http.StatusOK, PublicTenantResolveResponse{Found: false})
		return
	}
	metrics.IncPublicTenantResolve("found")
	body := PublicTenantResolveResponse{Found: true, Realm: res.Realm, Strategy: string(res.RealmType)}
	if res.RealmType == domain.RealmShared {
		body.TenantID = res.TenantID
	}
	c.JSON(http.StatusOK, body)
}
