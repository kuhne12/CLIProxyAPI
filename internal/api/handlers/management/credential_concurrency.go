package management

import (
	"github.com/gin-gonic/gin"
	"net/http"
)

func (h *Handler) GetCredentialConcurrency(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"credentials": h.authManager.CredentialConcurrency()})
}
