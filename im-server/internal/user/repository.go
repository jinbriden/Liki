package user

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Repository interface {
	Create(ctx context.Context, user *User) error
	FindByID(ctx context.Context, id int64) (*User, error)
	// FindByIDs 批量查询，不存在的 ID 直接跳过。只取资料字段，不读 password_hash。
	FindByIDs(ctx context.Context, ids []int64) ([]User, error)
	// SearchByUsernameOrID 查 lower(username) = lower(username) 或 id = id 的用户，最多 limit 条。
	// id 传 0 表示不按 ID 匹配（0 不在合法 ID 范围内，CHECK 约束保证不会命中）。
	SearchByUsernameOrID(ctx context.Context, username string, id int64, limit int) ([]User, error)
}

// profileColumns 查询用户资料时只取这几列，不把 password_hash 读进内存
var profileColumns = []string{"id", "username", "avatar_url"}

type gormRepository struct {
	db *gorm.DB // *gorm.DB 是 gorm 的数据库连接对象，用于执行数据库操作
}

func NewRepository(db *gorm.DB) Repository {
	return &gormRepository{db: db}
}

// Create 插入用户，u.ID 由调用方预先生成。
// users 表上唯一的唯一约束是主键，所以重复键错误一定是 ID 冲突。
func (r *gormRepository) Create(ctx context.Context, u *User) error {
	err := r.db.WithContext(ctx).Create(u).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrDuplicateID
	}
	return err
}

// FindByID 按主键查询，查不到时返回 ErrUserNotFound。
func (r *gormRepository) FindByID(ctx context.Context, id int64) (*User, error) {
	var u User
	// First 生成参数化查询：SELECT ... WHERE id = $1 ORDER BY id LIMIT 1
	err := r.db.WithContext(ctx).First(&u, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (r *gormRepository) FindByIDs(ctx context.Context, ids []int64) ([]User, error) {
	if len(ids) == 0 {
		return nil, nil // 避免生成 WHERE id IN () 这样的非法 SQL
	}
	var users []User
	// GORM 把切片参数展开成 IN ($1, $2, ...)，仍然是参数化查询
	err := r.db.WithContext(ctx).Select(profileColumns).Where("id IN ?", ids).Find(&users).Error
	return users, err
}

func (r *gormRepository) SearchByUsernameOrID(ctx context.Context, username string, id int64, limit int) ([]User, error) {
	var users []User
	// 两个条件分别命中 idx_users_username_lower 和主键索引，PostgreSQL 可以用 BitmapOr 合并。
	// ID 精确命中的排在最前面，其余按 ID 排序，保证结果稳定。
	err := r.db.WithContext(ctx).
		Select(profileColumns).
		Where("lower(username) = lower(?) OR id = ?", username, id).
		Order(clause.OrderBy{Expression: clause.Expr{SQL: "id = ? DESC, id", Vars: []any{id}}}).
		Limit(limit).
		Find(&users).Error
	return users, err
}
