package user

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

type Repository interface {
	Create(ctx context.Context, user *User) error
	FindByID(ctx context.Context, id int64) (*User, error)
}

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
