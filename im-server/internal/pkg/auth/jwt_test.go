package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "test-secret-0123456789-abcdefghijklmnop" // >= 32 字节

func newTestManager(t *testing.T) *JWTManager {
	t.Helper()
	m, err := NewJWTManager(testSecret, 72*time.Hour)
	if err != nil {
		t.Fatalf("NewJWTManager: %v", err)
	}
	return m
}

func TestNewJWTManager_Validation(t *testing.T) {
	if _, err := NewJWTManager(strings.Repeat("x", MinSecretLen-1), time.Hour); err == nil {
		t.Error("密钥不足 32 字节应当报错")
	}
	if _, err := NewJWTManager("", time.Hour); err == nil {
		t.Error("空密钥应当报错")
	}
	if _, err := NewJWTManager(testSecret, 0); err == nil {
		t.Error("ttl <= 0 应当报错")
	}
}

func TestIssueAndParse(t *testing.T) {
	m := newTestManager(t)
	fixed := time.Date(2026, 11, 21, 10, 0, 0, 500, time.UTC)
	m.now = func() time.Time { return fixed }

	token, exp, err := m.Issue(1234567890)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if want := fixed.Truncate(time.Second).Add(72 * time.Hour); !exp.Equal(want) {
		t.Errorf("expiresAt = %v, want %v", exp, want)
	}

	uid, err := m.Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if uid != 1234567890 {
		t.Errorf("uid = %d, want 1234567890", uid)
	}
}

func TestParse_Expired(t *testing.T) {
	m := newTestManager(t)
	issuedAt := time.Now()
	m.now = func() time.Time { return issuedAt }
	token, _, err := m.Issue(1234567890)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// 刚好过期前 1 秒仍然有效
	m.now = func() time.Time { return issuedAt.Add(72*time.Hour - time.Second) }
	if _, err := m.Parse(token); err != nil {
		t.Errorf("过期前应当有效: %v", err)
	}
	// 过期后被拒绝
	m.now = func() time.Time { return issuedAt.Add(72*time.Hour + time.Second) }
	if _, err := m.Parse(token); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("过期后 err = %v, want ErrInvalidToken", err)
	}
}

func TestParse_Rejects(t *testing.T) {
	m := newTestManager(t)
	validClaims := func() claims {
		now := time.Now()
		return claims{UserID: 1234567890, RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		}}
	}
	sign := func(method jwt.SigningMethod, key any, c claims) string {
		s, err := jwt.NewWithClaims(method, c).SignedString(key)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return s
	}

	noExp := validClaims()
	noExp.ExpiresAt = nil
	wrongIssuer := validClaims()
	wrongIssuer.Issuer = "someone-else"
	zeroUID := validClaims()
	zeroUID.UserID = 0

	tests := []struct {
		name  string
		token string
	}{
		{"空字符串", ""},
		{"乱写的字符串", "not-a-jwt"},
		{"错误的密钥签名", sign(jwt.SigningMethodHS256, []byte("another-secret-0123456789-abcdefgh"), validClaims())},
		{"HS512 算法", sign(jwt.SigningMethodHS512, []byte(testSecret), validClaims())},
		{"alg=none 未签名", sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, validClaims())},
		{"缺少 exp", sign(jwt.SigningMethodHS256, []byte(testSecret), noExp)},
		{"issuer 不对", sign(jwt.SigningMethodHS256, []byte(testSecret), wrongIssuer)},
		{"uid 为 0", sign(jwt.SigningMethodHS256, []byte(testSecret), zeroUID)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := m.Parse(tt.token); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("err = %v, want ErrInvalidToken", err)
			}
		})
	}

	// 篡改 payload（签名不变）也必须被拒绝
	t.Run("篡改payload", func(t *testing.T) {
		token, _, _ := m.Issue(1234567890)
		other, _, _ := m.Issue(1999999999)
		parts, otherParts := strings.Split(token, "."), strings.Split(other, ".")
		tampered := parts[0] + "." + otherParts[1] + "." + parts[2]
		if _, err := m.Parse(tampered); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("err = %v, want ErrInvalidToken", err)
		}
	})
}
