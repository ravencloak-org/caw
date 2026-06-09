package hub

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ravencloak-org/caw/internal/auth"
	"github.com/ravencloak-org/caw/internal/store"
)

// RequireRepoScope is a Gin middleware that enforces installation→repo scoping
// (ADR-0003). It must be placed after auth.Required so that the installation id
// is already present in the Gin context.
//
// The middleware:
//  1. Reads the installation id from auth.ContextInstallationID; returns 403 if
//     absent or equal to "setup" (the bootstrap-only sentinel value).
//  2. Reads the :owner and :repo path parameters and builds the full repo name.
//  3. Calls store.RepoInInstallation to confirm the repo belongs to this
//     installation; returns 500 on store error or 403 if not associated.
func RequireRepoScope(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		installationID, _ := c.Get(auth.ContextInstallationID)
		holder, ok := installationID.(string)
		if !ok || holder == "" || holder == "setup" {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}

		owner := c.Param("owner")
		repo := c.Param("repo")
		fullName := owner + "/" + repo

		associated, err := st.RepoInInstallation(holder, fullName)
		if err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		if !associated {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "installation not associated with this repository",
			})
			return
		}

		c.Next()
	}
}
