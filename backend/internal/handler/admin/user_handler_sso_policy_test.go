package admin

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAdminUserHTTPManagedIdentityRestrictions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := service.NewSettingService(&settingHandlerRepoStub{values: map[string]string{service.SettingKeySSOOnlyEnabled: "true"}}, &config.Config{})
	admin := newStubAdminService()
	admin.users = append(admin.users, service.User{ID: 2, Role: service.RoleAdmin, Email: "admin@example.com"})
	h := NewUserHandler(admin, nil, nil, nil, nil, nil, settings)
	router := gin.New()
	router.POST("/users", h.Create)
	router.PUT("/users/:id", h.Update)
	for _, field := range []string{"email", "password", "username", "avatar_url", "avatar", "email_verified", "email_verified_at", "auth_bindings", "auth_identities", "nickname", "display_name", "oauth_identities", "identities", "identity_bindings", "Username", "AVATAR_URL", "Email", "paſſword", "uſername"} {
		t.Run(field, func(t *testing.T) {
			payload := map[string]any{field: nil, "notes": "must not save"}
			rec := doJSON(t, router, http.MethodPut, "/users/1", payload)
			require.Equal(t, http.StatusForbidden, rec.Code)
			require.Contains(t, rec.Body.String(), "SSO_ONLY")
		})
	}
	rec := doJSON(t, router, http.MethodPut, "/users/1", map[string]any{"username": "new", "role": "admin"})
	require.Equal(t, http.StatusForbidden, rec.Code, "role promotion cannot bypass the identity guard")
	rec = doJSON(t, router, http.MethodPut, "/users/1", map[string]any{"notes": "business note"})
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doJSON(t, router, http.MethodPut, "/users/2", map[string]any{"email": "admin2@example.com"})
	require.Equal(t, http.StatusOK, rec.Code)
	for _, role := range []string{"", service.RoleUser} {
		rec = doJSON(t, router, http.MethodPost, "/users", map[string]any{"email": "new@example.com", "password": "password", "role": role})
		require.Equal(t, http.StatusForbidden, rec.Code)
	}
}
