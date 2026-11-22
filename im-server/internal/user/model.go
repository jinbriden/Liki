package user

import (
	"errors"
	"time"
)

// 业务哨兵错误，由 service / repository 返回，handler 负责翻译成错误码和提示
var (
	ErrInvalidUsername = errors.New("invalid username")
	ErrInvalidPassword = errors.New("invalid password")
	// ErrInvalidCredentials 登录失败。用户不存在和密码错误都返回它，不让调用方区分。
	ErrInvalidCredentials = errors.New("invalid user id or password")
	ErrUserNotFound       = errors.New("user not found")
	// ErrDuplicateID 插入时用户 ID 与已有账号冲突，service 会换一个 ID 重试
	ErrDuplicateID = errors.New("duplicate user id")
)

// User 对应 users 表。
// ID 是随机 10 位数，唯一标识账号（登录也使用 ID）；Username 不唯一，同时作为昵称展示。
type User struct {
	ID           int64 `gorm:"primaryKey;autoIncrement:false"`
	Username     string
	PasswordHash string
	AvatarURL    string `gorm:"column:avatar_url"`
	// CreatedAt 由数据库 now() 生成（精度微秒），插入时通过 RETURNING 取回，
	// 保证返回给调用方的值和库里存的完全一致。关闭 GORM 默认的"用 Go time.Now() 填充"。
	CreatedAt time.Time `gorm:"autoCreateTime:false;default:now()"`
}

// Profile 对其他模块公开的用户资料，不含 PasswordHash 等敏感字段。
// 好友、消息等模块只通过它拿用户信息，不直接查 users 表。
type Profile struct {
	ID        int64
	Username  string
	AvatarURL string
}
