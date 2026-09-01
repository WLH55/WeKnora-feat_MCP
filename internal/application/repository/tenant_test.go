package repository

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setupTestDB creates an in-memory SQLite database with tenant table.
func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	// DeleteTenant 的清理路径会 UPDATE users.tenant_id，所以 users 表
	// 必须在迁移清单里。
	require.NoError(t, db.AutoMigrate(&types.Tenant{}, &types.TenantMember{}, &types.User{}))
	return db
}

func TestDeleteTenant_SoftDeletesMemberships(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewTenantRepository(db)

	tenant := &types.Tenant{Name: "gone", Status: "active"}
	require.NoError(t, db.Create(tenant).Error)

	member := &types.TenantMember{
		UserID:   "user-1",
		TenantID: tenant.ID,
		Role:     types.TenantRoleOwner,
		Status:   types.TenantMemberStatusActive,
	}
	require.NoError(t, db.Create(member).Error)

	require.NoError(t, repo.DeleteTenant(ctx, tenant.ID))

	var tenantCount int64
	require.NoError(t, db.Model(&types.Tenant{}).Count(&tenantCount).Error)
	assert.Equal(t, int64(0), tenantCount)

	var memberCount int64
	require.NoError(t, db.Model(&types.TenantMember{}).Count(&memberCount).Error)
	assert.Equal(t, int64(0), memberCount)

	// Unscoped: rows still exist but are soft-deleted.
	var rawTenantCount int64
	require.NoError(t, db.Unscoped().Model(&types.Tenant{}).Count(&rawTenantCount).Error)
	assert.Equal(t, int64(1), rawTenantCount)

	var rawMemberCount int64
	require.NoError(t, db.Unscoped().Model(&types.TenantMember{}).Count(&rawMemberCount).Error)
	assert.Equal(t, int64(1), rawMemberCount)
}

func TestDeleteTenant_ClearsHomeTenantPointer(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewTenantRepository(db)

	tenant := &types.Tenant{Name: "home", Status: "active"}
	require.NoError(t, db.Create(tenant).Error)
	other := &types.Tenant{Name: "other", Status: "active"}
	require.NoError(t, db.Create(other).Error)

	homeUser := &types.User{ID: "home-user", Username: "h", Email: "h@example.com", PasswordHash: "x", TenantID: tenant.ID, IsActive: true}
	require.NoError(t, db.Create(homeUser).Error)
	otherUser := &types.User{ID: "other-user", Username: "o", Email: "o@example.com", PasswordHash: "x", TenantID: other.ID, IsActive: true}
	require.NoError(t, db.Create(otherUser).Error)

	require.NoError(t, repo.DeleteTenant(ctx, tenant.ID))

	// 悬空 home 指针必须被置 NULL（与 tenantless 惯例一致），否则登录
	// 仍会解析到已软删的空间。
	var nullCount int64
	require.NoError(t, db.Table("users").Where("id = ? AND tenant_id IS NULL", "home-user").Count(&nullCount).Error)
	assert.Equal(t, int64(1), nullCount)

	// 其他空间的 home 指针不受影响。
	var keepCount int64
	require.NoError(t, db.Table("users").Where("id = ? AND tenant_id = ?", "other-user", other.ID).Count(&keepCount).Error)
	assert.Equal(t, int64(1), keepCount)
}
