package service

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
)

// Preserve a recognized client's coherent UA/version pair only when explicitly
// selected by the administrator. Credential and account namespaces stay owned
// by the gateway; this does not forward attestation or arbitrary client headers.
func applyCodexIdentityPolicy(c *gin.Context, account *Account, h http.Header) {
	p := account.codexRequestPolicy()
	if !p.Enabled || p.IdentityMode != "preserve" || c == nil {
		return
	}
	ua := strings.TrimSpace(c.GetHeader("User-Agent"))
	if len(ua) > 512 {
		return
	}
	originator, paired, ok := openai.PairCodexClientIdentity(ua)
	if !ok {
		return
	}
	version := NormalizeCodexClientVersion(openai.CodexUserAgentVersion(paired))
	if version == "" || CompareVersions(version, codexUpstreamMinVersion) < 0 {
		return
	}
	h.Set("User-Agent", paired)
	h.Set("originator", originator)
	h.Set("version", version)
}
