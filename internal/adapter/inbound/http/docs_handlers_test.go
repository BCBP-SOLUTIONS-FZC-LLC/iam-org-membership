package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAsyncAPIYAMLHandler_ServesRawSpec(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi.yaml", nil)

	AsyncAPIYAMLHandler(c)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/yaml")
	body := w.Body.Bytes()
	assert.True(t, len(body) > 100, "embedded YAML should be non-trivial")
	assert.Contains(t, string(body), "asyncapi:",
		"raw spec must start with the AsyncAPI version key")
}

func TestSwaggerInitializerHandler_ServesJS(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "/swagger-initializer.js", nil)

	SwaggerInitializerHandler(c)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/javascript")
	assert.Contains(t, w.Body.String(), "SwaggerUIBundle",
		"initializer must reference the SwaggerUI bootstrap")
}

func TestSwaggerThemeHandler_ServesCSS(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "/swagger-theme.css", nil)

	SwaggerThemeHandler(c)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/css")
	// Sanity: the CSS references Poppins font family (bootstrap import at
	// the top of swaggerThemeCSS).
	assert.Contains(t, w.Body.String(), "Poppins")
}
