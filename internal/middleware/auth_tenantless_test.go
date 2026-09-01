package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

func TestTenantOptionalAPISurface(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodGet, "/api/v1/auth/me", true},
		{http.MethodPut, "/api/v1/auth/me", true},
		{http.MethodPut, "/api/v1/auth/me/preferences", true},
		{http.MethodPost, "/api/v1/tenants", true},
		{http.MethodGet, "/api/v1/me/invitations", true},
		{http.MethodPost, "/api/v1/me/invitations/12/accept", true},
		{http.MethodGet, "/api/v1/knowledge-bases", false},
		{http.MethodGet, "/api/v1/tenants", false},
	}
	for _, tt := range tests {
		if got := isTenantOptionalAPI(tt.path, tt.method); got != tt.want {
			t.Errorf("isTenantOptionalAPI(%s %s) = %v, want %v", tt.method, tt.path, got, tt.want)
		}
	}
}

func TestResolveFirstMembershipTarget(t *testing.T) {
	members := newFakeMemberService()
	members.seedActive("tenantless-user", 42, types.TenantRoleViewer)
	tenants := &fakeTenantService{tenant: &types.Tenant{ID: 42}}

	got := resolveFirstMembershipTarget(
		context.Background(),
		&types.User{ID: "tenantless-user"},
		members,
		tenants,
	)
	if got != 42 {
		t.Fatalf("resolved tenant = %d, want 42", got)
	}
}

// selectiveTenantService answers GetTenantByID from a map; every id not in
// the map behaves like a soft-deleted tenant ("record not found").
type selectiveTenantService struct {
	fakeTenantService
	byID map[uint64]*types.Tenant
}

func (f *selectiveTenantService) GetTenantByID(_ context.Context, id uint64) (*types.Tenant, error) {
	if tenant, ok := f.byID[id]; ok && tenant != nil {
		return tenant, nil
	}
	return nil, fmt.Errorf("record not found")
}

func TestRecoverDeadTenantSessionFallsBackToMembership(t *testing.T) {
	// JWT 仍指向已删除的空间 7，但用户在空间 42 有活跃成员关系 →
	// 会话直接恢复为空间 42，而不是把账号锁死在引导页。
	members := newFakeMemberService()
	members.seedActive("stranded", 42, types.TenantRoleOwner)
	tenants := &selectiveTenantService{byID: map[uint64]*types.Tenant{
		42: {ID: 42, Name: "alive", Status: "active"},
	}}

	got := recoverDeadTenantSession(context.Background(), &types.User{ID: "stranded"}, 7, tenants, members)
	if got == nil || got.ID != 42 {
		t.Fatalf("recovered tenant = %v, want tenant 42", got)
	}
}

func TestRecoverDeadTenantSessionNoMembershipReturnsNil(t *testing.T) {
	// 无任何活跃成员关系 → 无法恢复，调用方走 tenantless 语义。
	members := newFakeMemberService()
	tenants := &selectiveTenantService{byID: map[uint64]*types.Tenant{}}

	if got := recoverDeadTenantSession(context.Background(), &types.User{ID: "stranded"}, 7, tenants, members); got != nil {
		t.Fatalf("recovered tenant = %v, want nil", got)
	}
}

func TestRecoverDeadTenantSessionMembershipTenantAlsoGone(t *testing.T) {
	// 成员关系存在但其空间也已删除 → 跳过该成员关系，返回 nil。
	members := newFakeMemberService()
	members.seedActive("stranded", 42, types.TenantRoleOwner)
	tenants := &selectiveTenantService{byID: map[uint64]*types.Tenant{}}

	if got := recoverDeadTenantSession(context.Background(), &types.User{ID: "stranded"}, 7, tenants, members); got != nil {
		t.Fatalf("recovered tenant = %v, want nil", got)
	}
}

// runDeadTenantAuthRequest drives authenticateJWTUser end-to-end with a JWT
// scoped to tenant 7 that no longer exists, returning the middleware verdict,
// the HTTP status, and the tenant id the session finally landed on.
func runDeadTenantAuthRequest(
	t *testing.T,
	path string,
	members *fakeMemberService,
) (ok bool, status int, body string, landedTenant uint64) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	tenants := &selectiveTenantService{byID: map[uint64]*types.Tenant{
		42: {ID: 42, Name: "alive", Status: "active"},
	}}
	cfg := &config.Config{Tenant: &config.TenantConfig{}}
	user := &types.User{ID: "stranded", TenantID: 7}

	r := gin.New()
	r.Use(func(c *gin.Context) {
		ok = authenticateJWTUser(c, tenants, members, cfg, user, 7)
	})
	r.Any("/*rest", func(c *gin.Context) {
		status = http.StatusOK
		landedTenant, _ = types.TenantIDFromContext(c.Request.Context())
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if status != http.StatusOK {
		status = w.Code
		body = w.Body.String()
	}
	return ok, status, body, landedTenant
}

func TestAuthenticateJWTUserDeadTenantIdentityAPIPassesTenantless(t *testing.T) {
	// 验收标准 4 的前半：死空间 token + 无成员关系 → /auth/me 放行为
	// tenantless 会话（不是 401），会话不含租户上下文。
	members := newFakeMemberService()

	ok, status, body, landed := runDeadTenantAuthRequest(t, "/api/v1/auth/me", members)
	if !ok || status != http.StatusOK {
		t.Fatalf("ok=%v status=%d body=%s, want tenantless pass", ok, status, body)
	}
	if strings.Contains(body, "Unauthorized") {
		t.Fatalf("identity API must not 401 on dead tenant: %s", body)
	}
	if landed != 0 {
		t.Fatalf("landed tenant = %d, want 0 (tenantless)", landed)
	}
}

func TestAuthenticateJWTUserDeadTenantScopedAPIReturnsTenantRequired(t *testing.T) {
	// 验收标准 4 的后半：死空间 token + 无成员关系 → 空间级路由 409
	// TENANT_REQUIRED（不是 401，也不是放行）。
	members := newFakeMemberService()

	ok, status, body, _ := runDeadTenantAuthRequest(t, "/api/v1/knowledge-bases", members)
	if ok {
		t.Fatalf("scoped API must not pass without a tenant")
	}
	if status != http.StatusConflict || !strings.Contains(body, "TENANT_REQUIRED") {
		t.Fatalf("status=%d body=%s, want 409 TENANT_REQUIRED", status, body)
	}
	if strings.Contains(body, "Unauthorized") {
		t.Fatalf("must not degrade to 401: %s", body)
	}
}

func TestAuthenticateJWTUserDeadTenantRecoversIntoMembership(t *testing.T) {
	// 死空间 token + 在空间 42 有活跃成员关系 → 会话直接落进 42，
	// 空间级路由正常放行（等价于"接受邀请后无需重新登录"）。
	members := newFakeMemberService()
	members.seedActive("stranded", 42, types.TenantRoleOwner)

	ok, status, body, landed := runDeadTenantAuthRequest(t, "/api/v1/knowledge-bases", members)
	if !ok || status != http.StatusOK {
		t.Fatalf("ok=%v status=%d body=%s, want recovery into tenant 42", ok, status, body)
	}
	if landed != 42 {
		t.Fatalf("landed tenant = %d, want 42", landed)
	}
}
