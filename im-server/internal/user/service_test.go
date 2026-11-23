package user

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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
			env := newTestEnv()
			inv := env.newInvite(t, InviteOptions{})
			token := env.reserve(t, inv.Code)
			u, err := env.svc.Register(context.Background(), tt.username, tt.password, inv.Code, token)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				if got := env.repo.invite(inv.ID); got.UsedAt != nil || got.StateAt(time.Now()) != InviteReserved {
					t.Errorf("注册失败不应使用邀请码，冻结应保留（可以改正后再提交）：%+v", got)
				}
				return
			}
			if u.Status != StatusPending {
				t.Errorf("status = %q, 新账号应为待审核", u.Status)
			}
			if u.InviteCodeID == nil || *u.InviteCodeID != inv.ID {
				t.Errorf("invite_code_id = %v, want %d", u.InviteCodeID, inv.ID)
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
	env := newTestEnv()
	seen := map[int64]bool{}
	for _, name := range []string{"alice", "alice", "Alice"} {
		u := env.register(t, name)
		if seen[u.ID] {
			t.Fatalf("ID %d 重复", u.ID)
		}
		seen[u.ID] = true
	}
}

// 生成的 ID 和已有账号冲突时，换一个 ID 重试；冲突的那几次不消耗邀请码（事务回滚）
func TestService_Register_RetryOnDuplicateID(t *testing.T) {
	env := newTestEnv()
	const taken, fresh = 1111111111, 2222222222
	svc := NewService(env.repo, fakeTokens{}, fixedIDs(taken, taken, taken, fresh))

	ctx := context.Background()
	first := env.newInvite(t, InviteOptions{}).Code
	if _, err := svc.Register(ctx, "first", validPassword, first, env.reserve(t, first)); err != nil { // 占用 taken
		t.Fatalf("准备数据失败: %v", err)
	}
	inv := env.newInvite(t, InviteOptions{})
	u, err := svc.Register(ctx, "second", validPassword, inv.Code, env.reserve(t, inv.Code)) // taken 冲突两次后拿到 fresh
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if u.ID != fresh {
		t.Errorf("ID = %d, want %d", u.ID, fresh)
	}
	if got := env.repo.invite(inv.ID); got.UsedAt == nil || u.InviteCodeID == nil || *u.InviteCodeID != inv.ID {
		t.Errorf("邀请码应被这次注册使用：%+v", got)
	}
}

// 连续冲突达到上限后放弃，返回内部错误（不是业务错误）
func TestService_Register_GiveUpAfterMaxAttempts(t *testing.T) {
	env := newTestEnv()
	ids := make([]int64, maxIDAttempts+1)
	for i := range ids {
		ids[i] = 1111111111
	}
	svc := NewService(env.repo, fakeTokens{}, fixedIDs(ids...))

	ctx := context.Background()
	first := env.newInvite(t, InviteOptions{}).Code
	if _, err := svc.Register(ctx, "first", validPassword, first, env.reserve(t, first)); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	inv := env.newInvite(t, InviteOptions{})
	_, err := svc.Register(ctx, "second", validPassword, inv.Code, env.reserve(t, inv.Code))
	if err == nil {
		t.Fatal("连续冲突应当失败")
	}
	if errors.Is(err, ErrInvalidUsername) || errors.Is(err, ErrInvalidPassword) || errors.Is(err, ErrReservationLost) {
		t.Errorf("不应返回业务校验错误: %v", err)
	}
	if got := env.repo.invite(inv.ID); got.UsedAt != nil {
		t.Errorf("注册失败不应使用邀请码：%+v", got)
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
	env := newTestEnv()
	svc := env.svc
	ctx := context.Background()
	u := env.registerActive(t, validUsername)

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
	env := newTestEnv()
	ctx := context.Background()
	u := env.registerActive(t, validUsername)

	svc := NewService(env.repo, fakeTokens{err: errors.New("boom")}, counterIDs())
	_, err := svc.Login(ctx, u.ID, validPassword)
	if err == nil || errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("err = %v, want 内部错误（不能伪装成密码错误）", err)
	}
}

func TestService_GetByID(t *testing.T) {
	env := newTestEnv()
	svc := env.svc
	ctx := context.Background()
	u := env.register(t, validUsername)

	got, err := svc.GetByID(ctx, u.ID)
	if err != nil || got.ID != u.ID {
		t.Errorf("GetByID(%d) = %+v, %v", u.ID, got, err)
	}
	if _, err := svc.GetByID(ctx, 9999999999); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("err = %v, want ErrUserNotFound", err)
	}
}

func TestParseUserID(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"1234567890", 1234567890},
		{"9999999999", 9999999999},
		{"0123456789", 0},  // 以 0 开头，不在合法 ID 范围内
		{"123456789", 0},   // 9 位
		{"12345678901", 0}, // 11 位
		{"+123456789", 0},  // strconv.ParseInt 能解析，但不是纯数字
		{"12345abcde", 0},
		{"", 0},
	}
	for _, tt := range tests {
		if got := parseUserID(tt.in); got != tt.want {
			t.Errorf("parseUserID(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestService_SearchProfiles(t *testing.T) {
	env := newTestEnv()
	svc := env.svc
	// 只有已通过审核的账号能被搜到（FR-22），这里注册后都批准
	register := func(name string) int64 {
		t.Helper()
		return env.registerActive(t, name).ID
	}
	sam1 := register("Sam")
	sam2 := register("sam")
	_ = register("Samuel")
	// counterIDs 生成的第 4 个 ID 是 1000000004；再注册一个用户名恰好是这串数字的人
	digitID := register("1000000004")
	if digitID != 1000000004 {
		t.Fatalf("测试前提不成立：digitID = %d", digitID)
	}
	numericName := register("1000000001") // 用户名和 sam1 的 ID 相同

	tests := []struct {
		name    string
		keyword string
		limit   int
		want    []int64
	}{
		{name: "用户名不区分大小写、完全匹配", keyword: "SAM", limit: 20, want: []int64{sam1, sam2}},
		{name: "首尾空白", keyword: "  sam ", limit: 20, want: []int64{sam1, sam2}},
		{name: "ID 和纯数字用户名都命中，ID 命中的在前", keyword: "1000000001", limit: 20, want: []int64{sam1, numericName}},
		{name: "同一个人 ID 和用户名都命中，只出现一次", keyword: "1000000004", limit: 20, want: []int64{digitID}},
		{name: "limit 截断", keyword: "sam", limit: 1, want: []int64{sam1}},
		{name: "没有匹配", keyword: "nobody", limit: 20, want: nil},
		{name: "空关键字", keyword: "   ", limit: 20, want: nil},
		{name: "超长关键字", keyword: strings.Repeat("a", 33), limit: 20, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profiles, err := svc.SearchProfiles(context.Background(), tt.keyword, tt.limit)
			if err != nil {
				t.Fatalf("SearchProfiles: %v", err)
			}
			var got []int64
			for _, p := range profiles {
				got = append(got, p.ID)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestService_GetProfiles(t *testing.T) {
	env := newTestEnv()
	svc := env.svc
	u := env.register(t, validUsername)
	profiles, err := svc.GetProfiles(context.Background(), []int64{u.ID, 1999999999})
	if err != nil {
		t.Fatalf("GetProfiles: %v", err)
	}
	if len(profiles) != 1 || profiles[0].ID != u.ID || profiles[0].Username != validUsername {
		t.Errorf("profiles = %+v, want 只有 %d", profiles, u.ID)
	}
}

// denyAfter 前 n 次放行，之后一律拒绝；记录用过的键
type denyAfter struct {
	n    int
	keys []string
}

func (d *denyAfter) Allow(_ context.Context, key string) bool {
	d.keys = append(d.keys, key)
	return len(d.keys) <= d.n
}

// NFR-05：按目标账号限制登录尝试。成功、失败都计数，超出后连正确的密码也拒绝，且不再查库
func TestService_Login_RateLimited(t *testing.T) {
	ctx := context.Background()
	lim := &denyAfter{n: 2}
	env := newTestEnv(WithLoginLimiter(lim))
	svc := env.svc
	u := env.registerActive(t, validUsername)

	if _, err := svc.Login(ctx, u.ID, "Wrong1234"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("第 1 次：%v", err)
	}
	if _, err := svc.Login(ctx, u.ID, validPassword); err != nil {
		t.Fatalf("第 2 次（正确密码）：%v", err)
	}
	if _, err := svc.Login(ctx, u.ID, validPassword); !errors.Is(err, ErrTooManyAttempts) {
		t.Errorf("第 3 次应被限流：%v", err)
	}
	if want := strconv.FormatInt(u.ID, 10); lim.keys[0] != want {
		t.Errorf("限流的键应是目标用户 ID：%q, want %q", lim.keys[0], want)
	}
}
