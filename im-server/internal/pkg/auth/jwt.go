// Package auth 负责访问令牌（JWT）的签发与校验，以及 HTTP 鉴权中间件。
// 令牌的签发和解析不依赖 gin，WebSocket 网关建连时可以直接复用 Parse。
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	issuer = "liki"
	// MinSecretLen HS256 密钥的最小长度（字节）。密钥太短容易被离线暴力破解。
	MinSecretLen = 32
)

// ErrInvalidToken 令牌缺失、格式错误、签名不对、已过期等，对外统一表现为"未登录或登录已过期"
var ErrInvalidToken = errors.New("invalid token")

// claims 令牌里携带的数据。只放用户 ID，不放用户名等可变信息。
type claims struct {
	UserID int64 `json:"uid"`
	jwt.RegisteredClaims
}

type JWTManager struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time // 当前时间，测试时可以替换
}

func NewJWTManager(secret string, ttl time.Duration) (*JWTManager, error) {
	if len(secret) < MinSecretLen {
		return nil, fmt.Errorf("jwt secret must be at least %d bytes, got %d", MinSecretLen, len(secret))
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("jwt ttl must be positive, got %s", ttl)
	}
	return &JWTManager{secret: []byte(secret), ttl: ttl, now: time.Now}, nil
}

// Issue 为用户签发令牌，返回令牌字符串和过期时间。
func (m *JWTManager) Issue(userID int64) (string, time.Time, error) {
	// JWT 中的时间精确到秒，这里也截断到秒，保证返回的 expiresAt 和令牌里的 exp 一致
	now := m.now().Truncate(time.Second)
	exp := now.Add(m.ttl)

	c := claims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(m.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return token, exp, nil
}

// Parse 校验令牌并返回其中的用户 ID。任何校验失败都返回 ErrInvalidToken。
func (m *JWTManager) Parse(token string) (int64, error) {
	var c claims
	_, err := jwt.ParseWithClaims(token, &c,
		func(*jwt.Token) (any, error) { return m.secret, nil }, // 提供密钥给 jwt 包校验签名（匿名函数）
		// 只接受 HS256：防止"算法替换"攻击（如把 alg 改成 none 绕过签名校验）
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(m.now),
	)
	if err != nil || c.UserID <= 0 {
		return 0, ErrInvalidToken
	}
	return c.UserID, nil
}
