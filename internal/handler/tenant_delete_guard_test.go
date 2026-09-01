package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

// deleteGuardTenantService counts DeleteTenant calls so tests can assert
// the guard rejected the request before any destructive work happened.
type deleteGuardTenantService struct {
	interfaces.TenantService
	deleteCalls int
}

func (s *deleteGuardTenantService) DeleteTenant(context.Context, uint64) error {
	s.deleteCalls++
	return nil
}

type deleteGuardMemberService struct {
	interfaces.TenantMemberService
	members []*types.TenantMember
}

func (s *deleteGuardMemberService) ListByUser(context.Context, string) ([]*types.TenantMember, error) {
	return s.members, nil
}

func runDeleteGuardRequest(t *testing.T, tenantID uint64, user *types.User, members []*types.TenantMember) (*httptest.ResponseRecorder, *deleteGuardTenantService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	tenants := &deleteGuardTenantService{}
	h := &TenantHandler{
		service:       tenants,
		userService:   &tenantPolicyUserService{user: user},
		memberService: &deleteGuardMemberService{members: members},
	}
	r := gin.New()
	r.Use(tenantPolicyErrorCapture())
	r.DELETE("/tenants/:id", h.DeleteTenant)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/tenants/"+strconvUint(tenantID), nil)
	r.ServeHTTP(w, req)
	return w, tenants
}

func strconvUint(v uint64) string {
	if v == 0 {
		return "0"
	}
	buf := [20]byte{}
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func TestDeleteTenantDeniedWhenLastWorkspace(t *testing.T) {
	w, tenants := runDeleteGuardRequest(t, 5,
		&types.User{ID: "owner"},
		[]*types.TenantMember{
			{UserID: "owner", TenantID: 5, Role: types.TenantRoleOwner, Status: types.TenantMemberStatusActive},
		})

	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if tenants.deleteCalls != 0 {
		t.Fatalf("DeleteTenant called %d times, want 0", tenants.deleteCalls)
	}
	if !strings.Contains(w.Body.String(), `"code":2006`) {
		t.Fatalf("response missing typed last-workspace code: %s", w.Body.String())
	}
}

func TestDeleteTenantAllowedWithOtherMembership(t *testing.T) {
	w, tenants := runDeleteGuardRequest(t, 5,
		&types.User{ID: "owner"},
		[]*types.TenantMember{
			{UserID: "owner", TenantID: 5, Role: types.TenantRoleOwner, Status: types.TenantMemberStatusActive},
			{UserID: "owner", TenantID: 7, Role: types.TenantRoleAdmin, Status: types.TenantMemberStatusActive},
		})

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if tenants.deleteCalls != 1 {
		t.Fatalf("DeleteTenant called %d times, want 1", tenants.deleteCalls)
	}
}

func TestDeleteTenantGuardSkippedForCrossTenantSuperuser(t *testing.T) {
	// 超管管理空间目录，可能不是任何空间的成员；守卫不应拦截。
	w, tenants := runDeleteGuardRequest(t, 5,
		&types.User{ID: "super", CanAccessAllTenants: true},
		nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if tenants.deleteCalls != 1 {
		t.Fatalf("DeleteTenant called %d times, want 1", tenants.deleteCalls)
	}
}
