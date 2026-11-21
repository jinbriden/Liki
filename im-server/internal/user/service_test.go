package user

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// 满足所有规则的密码和用户名，用例里只改动被测的那一项
const (
	validUsername = "Alice"
	validPassword = "Passw0rd"
)

func TestService_Register(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
		wantErr  error
	}{
		{name: "正常注册", username: validUsername, password: validPassword},

		// 用户名
		{name: "用户名为空", username: "", password: validPassword, wantErr: ErrInvalidUsername},
		{name: "用户名2位", username: "ab", password: validPassword, wantErr: ErrInvalidUsername},
		{name: "用户名3位", username: "abc", password: validPassword},
		{name: "用户名32位", username: strings.Repeat("a", 32), password: validPassword},
		{name: "用户名33位", username: strings.Repeat("a", 33), password: validPassword, wantErr: ErrInvalidUsername},
		{name: "用户名含下划线", username: "a_b", password: validPassword, wantErr: ErrInvalidUsername},
		{name: "用户名含空格", username: "ali ce", password: validPassword, wantErr: ErrInvalidUsername},
		{name: "用户名含中文", username: "张三丰", password: validPassword, wantErr: ErrInvalidUsername},

		// 密码长度
		{name: "密码为空", username: validUsername, password: "", wantErr: ErrInvalidPassword},
		{name: "密码7位", username: validUsername, password: "Passw0r", wantErr: ErrInvalidPassword},
		{name: "密码8位", username: validUsername, password: "Passw0rd"},
		{name: "密码24位", username: validUsername, password: "Aa1" + strings.Repeat("x", 21)},
		{name: "密码25位", username: validUsername, password: "Aa1" + strings.Repeat("x", 22), wantErr: ErrInvalidPassword},

		// 密码字符组成
		{name: "缺大写字母", username: validUsername, password: "passw0rd", wantErr: ErrInvalidPassword},
		{name: "缺小写字母", username: validUsername, password: "PASSW0RD", wantErr: ErrInvalidPassword},
		{name: "缺数字", username: validUsername, password: "Password", wantErr: ErrInvalidPassword},
		{name: "含英文符号", username: validUsername, password: "P@ssw0rd!~"},
		{name: "含空格", username: validUsername, password: "Pass w0rd", wantErr: ErrInvalidPassword},
		{name: "含Tab", username: validUsername, password: "Pass\tw0rd", wantErr: ErrInvalidPassword},
		{name: "含中文", username: validUsername, password: "Passw0rd密", wantErr: ErrInvalidPassword},
		{name: "8个汉字(24字节)", username: validUsername, password: strings.Repeat("密", 8), wantErr: ErrInvalidPassword},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := newTestService()
			u, err := svc.Register(context.Background(), tt.username, tt.password)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if u.ID < minUserID || u.ID > maxUserID {
				t.Errorf("ID = %d, 不是 10 位数", u.ID)
			}
			if u.Username != tt.username {
				t.Errorf("username = %q, want 原样保存 %q", u.Username, tt.username)
			}
			if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(tt.password)); err != nil {
				t.Errorf("哈希与原密码不匹配: %v", err)
			}
		})
	}
}

// 用户名不去重：同名（含大小写不同）都能注册成功，靠不同的 ID 区分
func TestService_Register_DuplicateUsername(t *testing.T) {
	svc, _ := newTestService()
	seen := map[int64]bool{}
	for _, name := range []string{"alice", "alice", "Alice"} {
		u, err := svc.Register(context.Background(), name, validPassword)
		if err != nil {
			t.Fatalf("注册 %q 失败: %v", name, err)
		}
		if seen[u.ID] {
			t.Fatalf("ID %d 重复", u.ID)
		}
		seen[u.ID] = true
	}
}

// 生成的 ID 和已有账号冲突时，换一个 ID 重试
func TestService_Register_RetryOnDuplicateID(t *testing.T) {
	repo := newFakeRepo()
	const taken, fresh = 1111111111, 2222222222
	svc := NewService(repo, fakeTokens{}, fixedIDs(taken, taken, taken, fresh))

	ctx := context.Background()
	if _, err := svc.Register(ctx, "first", validPassword); err != nil { // 占用 taken
		t.Fatalf("准备数据失败: %v", err)
	}
	u, err := svc.Register(ctx, "second", validPassword) // taken 冲突两次后拿到 fresh
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if u.ID != fresh {
		t.Errorf("ID = %d, want %d", u.ID, fresh)
	}
}

// 连续冲突达到上限后放弃，返回内部错误（不是业务错误）
func TestService_Register_GiveUpAfterMaxAttempts(t *testing.T) {
	repo := newFakeRepo()
	ids := make([]int64, maxIDAttempts+1)
	for i := range ids {
		ids[i] = 1111111111
	}
	svc := NewService(repo, fakeTokens{}, fixedIDs(ids...))

	ctx := context.Background()
	if _, err := svc.Register(ctx, "first", validPassword); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	_, err := svc.Register(ctx, "second", validPassword)
	if err == nil {
		t.Fatal("连续冲突应当失败")
	}
	if errors.Is(err, ErrInvalidUsername) || errors.Is(err, ErrInvalidPassword) {
		t.Errorf("不应返回业务校验错误: %v", err)
	}
}

func TestRandomUserID(t *testing.T) {
	seen := map[int64]bool{}
	for range 10000 {
		id, err := RandomUserID()
		if err != nil {
			t.Fatalf("RandomUserID: %v", err)
		}
		if id < minUserID || id > maxUserID {
			t.Fatalf("ID = %d 超出 10 位数范围", id)
		}
		seen[id] = true
	}
	// 9e9 的空间里取 1 万个，几乎不可能大量重复；用来发现"随机数没生效"之类的问题
	if len(seen) < 9990 {
		t.Errorf("1 万次只得到 %d 个不同的 ID，随机性异常", len(seen))
	}
}

func TestService_Login(t *testing.T) {
	svc, _ := newTestService()
	ctx := context.Background()
	u, err := svc.Register(ctx, validUsername, validPassword)
	if err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	t.Run("登录成功", func(t *testing.T) {
		res, err := svc.Login(ctx, u.ID, validPassword)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if res.User.ID != u.ID || res.User.Username != validUsername {
			t.Errorf("user = %+v", res.User)
		}
		if res.Token != "token-for-"+strconv.FormatInt(u.ID, 10) {
			t.Errorf("token = %q, 应当为该用户签发", res.Token)
		}
	})

	// 以下情况都必须返回同一个 ErrInvalidCredentials，调用方无法区分是哪种原因
	failures := []struct {
		name     string
		userID   int64
		password string
	}{
		{"密码错误", u.ID, "Wr0ngPassword"},
		{"密码大小写不对", u.ID, "passw0rd"},
		{"用户不存在", 9999999999, validPassword},
		{"密码为空", u.ID, ""},
		{"密码超过24位", u.ID, validPassword + strings.Repeat("x", 17)},
		{"ID为0", 0, validPassword},
		{"ID为负数", -1, validPassword},
		{"ID不足10位", 123456789, validPassword},
		{"ID超过10位", 10000000000, validPassword},
	}
	for _, tt := range failures {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.Login(ctx, tt.userID, tt.password)
			if !errors.Is(err, ErrInvalidCredentials) {
				t.Errorf("err = %v, want ErrInvalidCredentials", err)
			}
		})
	}
}

func TestService_Login_IssueTokenFailed(t *testing.T) {
	repo := newFakeRepo()
	ctx := context.Background()
	u, err := NewService(repo, fakeTokens{}, counterIDs()).Register(ctx, validUsername, validPassword)
	if err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	svc := NewService(repo, fakeTokens{err: errors.New("boom")}, counterIDs())
	_, err = svc.Login(ctx, u.ID, validPassword)
	if err == nil || errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("err = %v, want 内部错误（不能伪装成密码错误）", err)
	}
}

func TestService_GetByID(t *testing.T) {
	svc, _ := newTestService()
	ctx := context.Background()
	u, _ := svc.Register(ctx, validUsername, validPassword)

	got, err := svc.GetByID(ctx, u.ID)
	if err != nil || got.ID != u.ID {
		t.Errorf("GetByID(%d) = %+v, %v", u.ID, got, err)
	}
	if _, err := svc.GetByID(ctx, 9999999999); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("err = %v, want ErrUserNotFound", err)
	}
}
