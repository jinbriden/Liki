package user

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRandomInviteCode(t *testing.T) {
	seen := map[string]bool{}
	chars := map[rune]int{}
	for range 10000 {
		code, err := RandomInviteCode()
		if err != nil {
			t.Fatalf("RandomInviteCode: %v", err)
		}
		if got, ok := NormalizeInviteCode(code); !ok || got != code {
			t.Fatalf("生成的码 %q 不是 6 位数字 / 大写字母", code)
		}
		seen[code] = true
		for _, c := range code {
			chars[c]++
		}
	}
	// 36^6 的空间里取 1 万个，几乎不会重复；36 个字符应该都出现过（每个字符期望出现约 1667 次）
	if len(seen) < 9990 {
		t.Errorf("1 万次只得到 %d 个不同的码，随机性异常", len(seen))
	}
	if len(chars) != len(inviteCodeAlphabet) {
		t.Errorf("只出现了 %d 种字符，want %d", len(chars), len(inviteCodeAlphabet))
	}
}

func TestNormalizeInviteCode(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"A7K2Q9", "A7K2Q9", true},
		{"a7k2q9", "A7K2Q9", true}, // 不区分大小写
		{"  A7K2Q9\n", "A7K2Q9", true},
		{"000000", "000000", true},
		{"", "", false},
		{"A7K2Q", "", false},
		{"A7K2Q9X", "", false},
		{"A7K-Q9", "", false},
		{"A7K Q9", "", false},
		{"Ａ7K2Q9", "", false}, // 全角字母
		{"邀请码12", "", false},
	}
	for _, tt := range tests {
		got, ok := NormalizeInviteCode(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("NormalizeInviteCode(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

// state 邀请码在 fakeRepo 当前时钟下的状态
func (e *testEnv) state(id int64) InviteState {
	c := e.repo.invite(id)
	return c.StateAt(e.repo.now())
}

// ---------- 验证与冻结（FR-23） ----------

func TestInviteService_Verify(t *testing.T) {
	ctx := context.Background()

	t.Run("有效的码：冻结，有效期不变", func(t *testing.T) {
		env := newTestEnv()
		inv := env.newInvite(t, InviteOptions{})
		r, err := env.invites.Verify(ctx, " "+strings.ToLower(inv.Code)+" ", env.nextIP()) // 不区分大小写
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Token) < 26 || !r.ExpiresAt.Equal(inv.ExpiresAt) {
			t.Errorf("reservation = %+v", r)
		}
		if d := r.ReservedUntil.Sub(env.repo.now()); d != ReservationLease {
			t.Errorf("冻结时长 = %s, want %s", d, ReservationLease)
		}
		if env.state(inv.ID) != InviteReserved {
			t.Errorf("state = %s, want reserved", env.state(inv.ID))
		}
		if got := env.repo.invite(inv.ID); *got.ReservationHash == r.Token {
			t.Error("库里不应存令牌明文")
		}
	})

	t.Run("冻结期间别人验证：使用中", func(t *testing.T) {
		env := newTestEnv()
		inv := env.newInvite(t, InviteOptions{})
		env.reserve(t, inv.Code)
		if _, err := env.invites.Verify(ctx, inv.Code, env.nextIP()); !errors.Is(err, ErrInviteInUse) {
			t.Errorf("err = %v, want ErrInviteInUse", err)
		}
	})

	t.Run("无效：不存在、格式不对、过期、已使用、已作废", func(t *testing.T) {
		env := newTestEnv()
		expired := env.newInvite(t, InviteOptions{})
		env.repo.editInvite(expired.ID, func(c *InviteCode) { c.ExpiresAt = env.repo.now().Add(-time.Second) })
		revoked := env.newInvite(t, InviteOptions{})
		if _, err := env.invites.Revoke(ctx, testAdminID, revoked.ID); err != nil {
			t.Fatal(err)
		}
		used := env.register(t, "alice")
		usedCode := env.repo.invite(*used.InviteCodeID).Code

		for name, code := range map[string]string{
			"不存在": "ZZZZZZ", "格式不对": "ABC-12", "空": "", "过期": expired.Code, "已作废": revoked.Code, "已使用": usedCode,
		} {
			if _, err := env.invites.Verify(ctx, code, env.nextIP()); !errors.Is(err, ErrInvalidInvite) {
				t.Errorf("%s：err = %v, want ErrInvalidInvite", name, err)
			}
		}
	})
}

// 每个码 1 分钟内最多验证成功 1 次、24 小时内最多 10 次
func TestInviteService_Verify_CodeRateLimit(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv()
	inv := env.newInvite(t, InviteOptions{})

	verify := func() (*Reservation, error) { return env.invites.Verify(ctx, inv.Code, env.nextIP()) }
	release := func(r *Reservation) {
		if err := env.invites.Release(ctx, inv.Code, r.Token, false); err != nil {
			t.Fatal(err)
		}
	}

	r, err := verify()
	if err != nil {
		t.Fatal(err)
	}
	release(r)
	if _, err := verify(); !errors.Is(err, ErrInviteVerifyTooFrequent) {
		t.Fatalf("解冻后 1 分钟内再验证：err = %v, want ErrInviteVerifyTooFrequent", err)
	}
	env.repo.advance(time.Minute + time.Second)

	// 加上第一次，24 小时内一共验证成功 10 次
	for i := 2; i <= 10; i++ {
		r, err := verify()
		if err != nil {
			t.Fatalf("第 %d 次：%v", i, err)
		}
		release(r)
		env.repo.advance(time.Minute + time.Second)
	}
	if _, err := verify(); !errors.Is(err, ErrInviteVerifyDailyLimit) {
		t.Fatalf("第 11 次：err = %v, want ErrInviteVerifyDailyLimit", err)
	}

	env.repo.advance(24 * time.Hour) // 滚动 24 小时：最早的记录移出窗口
	if _, err := verify(); err != nil {
		t.Errorf("24 小时后：%v", err)
	}
}

// "使用中"被拒绝的尝试不计入码的次数：知道码的人不能故意刷掉它的验证次数
func TestInviteService_Verify_InUseNotCounted(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv()
	inv := env.newInvite(t, InviteOptions{})
	holder := env.reserve(t, inv.Code)
	for range 20 {
		if _, err := env.invites.Verify(ctx, inv.Code, env.nextIP()); !errors.Is(err, ErrInviteInUse) {
			t.Fatalf("err = %v", err)
		}
	}
	if err := env.invites.Release(ctx, inv.Code, holder, false); err != nil {
		t.Fatal(err)
	}
	env.repo.advance(time.Minute + time.Second)
	if _, err := env.invites.Verify(ctx, inv.Code, env.nextIP()); err != nil {
		t.Errorf("被别人试了 20 次之后仍应能验证：%v", err)
	}
}

// 每个 IP 24 小时内最多验证 10 次，不论码是否有效（防止暴力猜码）
func TestInviteService_Verify_IPLimit(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv()
	const ip = "203.0.113.7"

	// 格式不对的不查库，不计数
	for range 5 {
		if _, err := env.invites.Verify(ctx, "bad", ip); !errors.Is(err, ErrInvalidInvite) {
			t.Fatal(err)
		}
	}
	for i := range 10 {
		if _, err := env.invites.Verify(ctx, "ZZZZZ"+string(rune('0'+i)), ip); !errors.Is(err, ErrInvalidInvite) {
			t.Fatalf("第 %d 次：%v", i+1, err)
		}
	}
	valid := env.newInvite(t, InviteOptions{})
	if _, err := env.invites.Verify(ctx, valid.Code, ip); !errors.Is(err, ErrVerifyIPLimit) {
		t.Fatalf("第 11 次（码有效）：err = %v, want ErrVerifyIPLimit", err)
	}
	if env.state(valid.ID) != InviteActive {
		t.Error("超出 IP 限制时不应冻结码")
	}
	if _, err := env.invites.Verify(ctx, valid.Code, "203.0.113.8"); err != nil {
		t.Errorf("别的 IP 不受影响：%v", err)
	}

	env.repo.advance(24*time.Hour + time.Second)
	other := env.newInvite(t, InviteOptions{})
	if _, err := env.invites.Verify(ctx, other.Code, ip); err != nil {
		t.Errorf("24 小时后：%v", err)
	}
}

func TestInviteService_RenewAndRelease(t *testing.T) {
	ctx := context.Background()

	t.Run("续期：冻结延长到 3 分钟后，有效期不变", func(t *testing.T) {
		env := newTestEnv()
		inv := env.newInvite(t, InviteOptions{})
		token := env.reserve(t, inv.Code)
		env.repo.advance(2 * time.Minute)
		r, err := env.invites.Renew(ctx, inv.Code, token)
		if err != nil {
			t.Fatal(err)
		}
		if d := r.ReservedUntil.Sub(env.repo.now()); d != ReservationLease || !r.ExpiresAt.Equal(inv.ExpiresAt) || r.Token != token {
			t.Errorf("reservation = %+v", r)
		}
	})

	t.Run("不续期：租约到期自动恢复可用", func(t *testing.T) {
		env := newTestEnv()
		inv := env.newInvite(t, InviteOptions{})
		token := env.reserve(t, inv.Code)
		env.repo.advance(ReservationLease + time.Second)
		if env.state(inv.ID) != InviteActive {
			t.Errorf("state = %s, want active", env.state(inv.ID))
		}
		if _, err := env.invites.Renew(ctx, inv.Code, token); !errors.Is(err, ErrReservationLost) {
			t.Errorf("到期后续期：err = %v", err)
		}
		if _, err := env.invites.Verify(ctx, inv.Code, env.nextIP()); err != nil {
			t.Errorf("到期后别人可以验证：%v", err)
		}
		if got := env.repo.invite(inv.ID); !got.ExpiresAt.Equal(inv.ExpiresAt) {
			t.Error("有效期不应因冻结、解冻而改变")
		}
	})

	t.Run("冻结最长 30 分钟", func(t *testing.T) {
		env := newTestEnv()
		inv := env.newInvite(t, InviteOptions{})
		token := env.reserve(t, inv.Code)
		for i := 1; i <= 14; i++ { // 每 2 分钟续期一次，一直续到第 28 分钟
			env.repo.advance(2 * time.Minute)
			if _, err := env.invites.Renew(ctx, inv.Code, token); err != nil {
				t.Fatalf("第 %d 分钟续期：%v", 2*i, err)
			}
		}
		r, err := env.invites.Renew(ctx, inv.Code, token)
		if err != nil {
			t.Fatal(err)
		}
		reservedAt := *env.repo.invite(inv.ID).ReservedAt
		if !r.ReservedUntil.Equal(reservedAt.Add(ReservationMaxHold)) {
			t.Errorf("临近上限时续期只能到 冻结时间 + 30 分钟：%s", r.ReservedUntil.Sub(reservedAt))
		}
		env.repo.advance(2 * time.Minute)
		if _, err := env.invites.Renew(ctx, inv.Code, token); !errors.Is(err, ErrReservationLost) {
			t.Errorf("超过 30 分钟：err = %v", err)
		}
	})

	t.Run("立即解冻", func(t *testing.T) {
		env := newTestEnv()
		inv := env.newInvite(t, InviteOptions{})
		token := env.reserve(t, inv.Code)
		if err := env.invites.Release(ctx, inv.Code, token, false); err != nil {
			t.Fatal(err)
		}
		if env.state(inv.ID) != InviteActive {
			t.Errorf("state = %s", env.state(inv.ID))
		}
		if _, err := env.invites.Renew(ctx, inv.Code, token); !errors.Is(err, ErrReservationLost) {
			t.Errorf("解冻后续期：err = %v", err)
		}
	})

	t.Run("软释放：15 秒内续期可恢复（刷新页面），否则恢复可用（关闭页面）", func(t *testing.T) {
		env := newTestEnv()
		inv := env.newInvite(t, InviteOptions{})
		token := env.reserve(t, inv.Code)
		if err := env.invites.Release(ctx, inv.Code, token, true); err != nil {
			t.Fatal(err)
		}
		env.repo.advance(10 * time.Second)
		if r, err := env.invites.Renew(ctx, inv.Code, token); err != nil || r.ReservedUntil.Sub(env.repo.now()) != ReservationLease {
			t.Fatalf("刷新后续期：%+v, %v", r, err)
		}
		if err := env.invites.Release(ctx, inv.Code, token, true); err != nil {
			t.Fatal(err)
		}
		env.repo.advance(ReservationGrace + time.Second)
		if env.state(inv.ID) != InviteActive {
			t.Errorf("关闭页面 15 秒后：state = %s, want active", env.state(inv.ID))
		}
	})

	t.Run("令牌不对：续期失败，解冻无效", func(t *testing.T) {
		env := newTestEnv()
		inv := env.newInvite(t, InviteOptions{})
		env.reserve(t, inv.Code)
		if _, err := env.invites.Renew(ctx, inv.Code, "WRONGTOKEN"); !errors.Is(err, ErrReservationLost) {
			t.Errorf("err = %v", err)
		}
		if err := env.invites.Release(ctx, inv.Code, "WRONGTOKEN", false); err != nil {
			t.Fatal(err)
		}
		if env.state(inv.ID) != InviteReserved {
			t.Error("别人的令牌不能解冻")
		}
	})
}

// ---------- 提交注册申请（FR-01、FR-23） ----------

func TestService_Register_Reservation(t *testing.T) {
	ctx := context.Background()

	t.Run("提交即失效，与审核结果无关", func(t *testing.T) {
		env := newTestEnv()
		inv := env.newInvite(t, InviteOptions{})
		token := env.reserve(t, inv.Code)
		u, err := env.svc.Register(ctx, "alice", validPassword, inv.Code, token)
		if err != nil {
			t.Fatal(err)
		}
		if u.Status != StatusPending || u.InviteCodeID == nil || *u.InviteCodeID != inv.ID {
			t.Errorf("user = %+v", u)
		}
		if env.state(inv.ID) != InviteUsed {
			t.Errorf("state = %s, want used", env.state(inv.ID))
		}
		if _, err := env.svc.Register(ctx, "bob", validPassword, inv.Code, token); !errors.Is(err, ErrReservationLost) {
			t.Errorf("同一个令牌再提交：err = %v", err)
		}
		// 被拒绝也不会让码恢复
		if err := env.review.Reject(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
		env.repo.advance(time.Minute + time.Second)
		if _, err := env.invites.Verify(ctx, inv.Code, env.nextIP()); !errors.Is(err, ErrInvalidInvite) {
			t.Errorf("已使用的码再验证：err = %v", err)
		}
	})

	t.Run("没有令牌、令牌不对：失败，不影响持有者", func(t *testing.T) {
		env := newTestEnv()
		inv := env.newInvite(t, InviteOptions{})
		token := env.reserve(t, inv.Code)
		for _, tok := range []string{"", "WRONGTOKEN"} {
			if _, err := env.svc.Register(ctx, "eve", validPassword, inv.Code, tok); !errors.Is(err, ErrReservationLost) {
				t.Errorf("令牌 %q：err = %v", tok, err)
			}
		}
		if _, err := env.svc.Register(ctx, "alice", validPassword, inv.Code, token); err != nil {
			t.Errorf("持有者提交：%v", err)
		}
	})

	t.Run("冻结到期、填表时过期、被作废：失败", func(t *testing.T) {
		env := newTestEnv()

		lapsed := env.newInvite(t, InviteOptions{})
		lapsedToken := env.reserve(t, lapsed.Code)
		env.repo.advance(ReservationLease + time.Second)
		if _, err := env.svc.Register(ctx, "aaa", validPassword, lapsed.Code, lapsedToken); !errors.Is(err, ErrReservationLost) {
			t.Errorf("冻结到期：err = %v", err)
		}

		expiring := env.newInvite(t, InviteOptions{TTL: time.Hour})
		expiringToken := env.reserve(t, expiring.Code)
		env.repo.editInvite(expiring.ID, func(c *InviteCode) { c.ExpiresAt = env.repo.now().Add(-time.Second) })
		if _, err := env.svc.Register(ctx, "bbb", validPassword, expiring.Code, expiringToken); !errors.Is(err, ErrReservationLost) {
			t.Errorf("填表时码过期（有效期不因冻结延长）：err = %v", err)
		}

		revoked := env.newInvite(t, InviteOptions{})
		revokedToken := env.reserve(t, revoked.Code)
		if _, err := env.invites.Revoke(ctx, testAdminID, revoked.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := env.svc.Register(ctx, "ccc", validPassword, revoked.Code, revokedToken); !errors.Is(err, ErrReservationLost) {
			t.Errorf("填表时码被作废：err = %v", err)
		}
	})

	t.Run("用户名、密码不合规：冻结保留，改正后可以再提交", func(t *testing.T) {
		env := newTestEnv()
		inv := env.newInvite(t, InviteOptions{})
		token := env.reserve(t, inv.Code)
		if _, err := env.svc.Register(ctx, "a_b", validPassword, inv.Code, token); !errors.Is(err, ErrInvalidUsername) {
			t.Errorf("err = %v", err)
		}
		if _, err := env.svc.Register(ctx, "alice", "weak", inv.Code, token); !errors.Is(err, ErrInvalidPassword) {
			t.Errorf("err = %v", err)
		}
		if env.state(inv.ID) != InviteReserved {
			t.Errorf("state = %s, want reserved", env.state(inv.ID))
		}
		if _, err := env.svc.Register(ctx, "alice", validPassword, inv.Code, token); err != nil {
			t.Errorf("改正后提交：%v", err)
		}
	})
}

// ---------- 登录时的审核状态（FR-22） ----------

func TestService_Login_ReviewStatus(t *testing.T) {
	env := newTestEnv()
	ctx := context.Background()
	u := env.register(t, validUsername)

	check := func(step, password string, want error) {
		t.Helper()
		_, err := env.svc.Login(ctx, u.ID, password)
		if !errors.Is(err, want) {
			t.Errorf("%s：err = %v, want %v", step, err, want)
		}
	}
	check("待审核", validPassword, ErrAccountPending)
	// 密码错误时一律是 ErrInvalidCredentials，不透露审核状态
	check("待审核 + 密码错误", "Wr0ngPass", ErrInvalidCredentials)

	if err := env.review.Reject(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	check("已拒绝", validPassword, ErrAccountRejected)
	check("已拒绝 + 密码错误", "Wr0ngPass", ErrInvalidCredentials)

	// 被拒绝后可以再批准
	if err := env.review.Approve(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	check("批准后", validPassword, nil)
}

// 只有已通过的账号能被搜到：按用户名、按 ID 都搜不到待审核 / 已拒绝的
func TestService_SearchProfiles_OnlyActive(t *testing.T) {
	env := newTestEnv()
	ctx := context.Background()
	active := env.registerActive(t, "sam")
	pending := env.register(t, "sam")
	rejected := env.register(t, "sam")
	if err := env.review.Reject(ctx, rejected.ID); err != nil {
		t.Fatal(err)
	}

	for _, kw := range []string{"sam", "1000000002", "1000000003"} {
		profiles, err := env.svc.SearchProfiles(ctx, kw, 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range profiles {
			if p.ID != active.ID {
				t.Errorf("搜索 %q 搜到了未通过审核的账号 %d（pending=%d, rejected=%d）", kw, p.ID, pending.ID, rejected.ID)
			}
		}
	}
}

// ---------- 生成、查看、作废 ----------

func TestInviteService_Create(t *testing.T) {
	ctx := context.Background()

	t.Run("默认 3 天过期", func(t *testing.T) {
		env := newTestEnv()
		c, err := env.invites.Create(ctx, testAdminID, InviteOptions{Note: "  给小王  "})
		if err != nil {
			t.Fatal(err)
		}
		if c.CreatedBy != testAdminID || c.Note != "给小王" || c.UsedAt != nil || c.ReservedUntil != nil {
			t.Errorf("invite = %+v", c)
		}
		if d := c.ExpiresAt.Sub(c.CreatedAt); d != 72*time.Hour {
			t.Errorf("有效期 = %s, want 72h", d)
		}
		if env.state(c.ID) != InviteActive {
			t.Errorf("state = %s", env.state(c.ID))
		}
	})

	t.Run("只有管理员能生成", func(t *testing.T) {
		env := newTestEnv()
		u := env.registerActive(t, "alice")
		if _, err := env.invites.Create(ctx, u.ID, InviteOptions{}); !errors.Is(err, ErrInviteForbidden) {
			t.Errorf("普通账号：err = %v, want ErrInviteForbidden", err)
		}
		noAdmin := NewInviteService(env.repo, 0, counterCodes())
		if _, err := noAdmin.Create(ctx, testAdminID, InviteOptions{}); !errors.Is(err, ErrInviteForbidden) {
			t.Errorf("没配置管理员：err = %v, want ErrInviteForbidden", err)
		}
		if noAdmin.CanInvite(0) {
			t.Error("没配置管理员时，ID 0 不能被当成管理员")
		}
	})

	t.Run("参数校验", func(t *testing.T) {
		env := newTestEnv()
		tests := []struct {
			name string
			opts InviteOptions
			want error
		}{
			{"备注 50 个汉字", InviteOptions{Note: strings.Repeat("字", 50)}, nil},
			{"备注 51 个汉字", InviteOptions{Note: strings.Repeat("字", 51)}, ErrInvalidInviteNote},
			{"有效期 1 小时", InviteOptions{TTL: time.Hour}, nil},
			{"有效期 59 分钟", InviteOptions{TTL: 59 * time.Minute}, ErrInvalidInviteOptions},
			{"有效期 30 天", InviteOptions{TTL: 30 * 24 * time.Hour}, nil},
			{"有效期 31 天", InviteOptions{TTL: 31 * 24 * time.Hour}, ErrInvalidInviteOptions},
			{"有效期为负", InviteOptions{TTL: -time.Hour}, ErrInvalidInviteOptions},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if _, err := env.invites.Create(ctx, testAdminID, tt.opts); !errors.Is(err, tt.want) {
					t.Errorf("err = %v, want %v", err, tt.want)
				}
			})
		}
	})

	t.Run("码重复时重新生成", func(t *testing.T) {
		repo := newTestRepo()
		svc := NewInviteService(repo, testAdminID, fixedCodes("AAAAAA", "AAAAAA", "AAAAAA", "BBBBBB"))
		if c, _ := svc.Create(ctx, testAdminID, InviteOptions{}); c.Code != "AAAAAA" {
			t.Fatalf("code = %q", c.Code)
		}
		c, err := svc.Create(ctx, testAdminID, InviteOptions{})
		if err != nil || c.Code != "BBBBBB" {
			t.Errorf("code = %v, err = %v; want BBBBBB", c, err)
		}
	})
}

// 溯源（FR-23）：每个码能查到谁用它注册、谁验证过；每个账号能查到邀请它的码和生成者
func TestInviteService_ListAndTrace(t *testing.T) {
	env := newTestEnv()
	ctx := context.Background()
	first := env.newInvite(t, InviteOptions{Note: "first"})
	second := env.newInvite(t, InviteOptions{Note: "second"})

	token := env.reserve(t, first.Code)
	if _, err := env.invites.Verify(ctx, first.Code, "203.0.113.9"); !errors.Is(err, ErrInviteInUse) { // 别人也来试
		t.Fatal(err)
	}
	alice, err := env.svc.Register(ctx, "alice", validPassword, first.Code, token)
	if err != nil {
		t.Fatal(err)
	}

	list, err := env.invites.List(ctx, testAdminID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("应按生成时间倒序：%+v", list)
	}
	if list[0].UsedBy != nil || len(list[0].Verifications) != 0 {
		t.Errorf("second 没人用过：%+v", list[0])
	}
	used := list[1]
	if used.UsedBy == nil || used.UsedBy.ID != alice.ID || used.UsedBy.PasswordHash != "" {
		t.Errorf("first.usedBy = %+v", used.UsedBy)
	}
	var results []VerifyResult
	for _, v := range used.Verifications {
		results = append(results, v.Result)
	}
	if !slices.Equal(results, []VerifyResult{VerifyInUse, VerifyReserved}) || used.Verifications[0].IP != "203.0.113.9" {
		t.Errorf("验证记录（最新的在前）= %+v", used.Verifications)
	}

	// 反向：账号 → 邀请码 → 生成者
	items, err := env.review.List(ctx, StatusPending)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Invite == nil || items[0].Invite.ID != first.ID || items[0].Invite.CreatedBy != testAdminID {
		t.Errorf("账号溯源不到邀请码：%+v", items)
	}

	// 普通账号看不到管理员的码
	if list, _ := env.invites.List(ctx, alice.ID); len(list) != 0 {
		t.Errorf("alice 的列表应为空：%+v", list)
	}
}

func TestInviteService_Revoke(t *testing.T) {
	env := newTestEnv()
	ctx := context.Background()

	inv := env.newInvite(t, InviteOptions{})
	d, err := env.invites.Revoke(ctx, testAdminID, inv.ID)
	if err != nil || d.RevokedAt == nil || d.StateAt(env.repo.now()) != InviteRevoked {
		t.Fatalf("作废：%+v, %v", d, err)
	}
	revokedAt := *d.RevokedAt
	// 再作废一次：幂等，作废时间不变
	d, err = env.invites.Revoke(ctx, testAdminID, inv.ID)
	if err != nil || !d.RevokedAt.Equal(revokedAt) {
		t.Errorf("重复作废：%+v, %v", d, err)
	}

	// 冻结中的可以作废，冻结随之失效
	held := env.newInvite(t, InviteOptions{})
	env.reserve(t, held.Code)
	if d, err := env.invites.Revoke(ctx, testAdminID, held.ID); err != nil || d.ReservedUntil != nil {
		t.Errorf("作废冻结中的码：%+v, %v", d, err)
	}

	// 已使用的码不再标记作废，状态仍是"已使用"
	u := env.register(t, "alice")
	d, err = env.invites.Revoke(ctx, testAdminID, *u.InviteCodeID)
	if err != nil || d.RevokedAt != nil || d.StateAt(env.repo.now()) != InviteUsed || d.UsedBy == nil {
		t.Errorf("作废已使用的码：%+v, %v", d, err)
	}

	// 不存在、不是自己的
	if _, err := env.invites.Revoke(ctx, testAdminID, 999); !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("不存在：err = %v", err)
	}
	other := env.newInvite(t, InviteOptions{})
	if _, err := env.invites.Revoke(ctx, 1000000001, other.ID); !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("不是自己的：err = %v", err)
	}
	if got := env.repo.invite(other.ID); got.RevokedAt != nil {
		t.Errorf("别人的码被作废了")
	}
}

func TestInviteService_VerifyAdmin(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo()
	if err := NewInviteService(repo, testAdminID, counterCodes()).VerifyAdmin(ctx); err != nil {
		t.Errorf("正常配置：%v", err)
	}
	if err := NewInviteService(repo, 0, counterCodes()).VerifyAdmin(ctx); err == nil {
		t.Error("没配置管理员应当报错")
	}
	if err := NewInviteService(repo, 1999999999, counterCodes()).VerifyAdmin(ctx); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("账号不存在：%v", err)
	}
	repo.setStatus(testAdminID, StatusPending)
	if err := NewInviteService(repo, testAdminID, counterCodes()).VerifyAdmin(ctx); err == nil {
		t.Error("管理员账号待审核时应当报错")
	}
}

func TestInviteCode_StateAt(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Minute), now.Add(time.Minute)
	tests := []struct {
		name string
		c    InviteCode
		want InviteState
	}{
		{"可用", InviteCode{ExpiresAt: future}, InviteActive},
		{"冻结中", InviteCode{ExpiresAt: future, ReservedUntil: &future}, InviteReserved},
		{"冻结已到期：可用", InviteCode{ExpiresAt: future, ReservedUntil: &past}, InviteActive},
		{"已使用", InviteCode{ExpiresAt: future, UsedAt: &past}, InviteUsed},
		{"过期", InviteCode{ExpiresAt: past}, InviteExpired},
		{"恰好到期", InviteCode{ExpiresAt: now}, InviteExpired},
		{"作废", InviteCode{ExpiresAt: future, RevokedAt: &past}, InviteRevoked},
		{"冻结中但已过期：显示过期", InviteCode{ExpiresAt: past, ReservedUntil: &future}, InviteExpired},
		{"已使用又过期：显示已使用", InviteCode{ExpiresAt: past, UsedAt: &past}, InviteUsed},
		{"作废又过期：显示作废", InviteCode{ExpiresAt: past, RevokedAt: &past}, InviteRevoked},
	}
	for _, tt := range tests {
		if got := tt.c.StateAt(now); got != tt.want {
			t.Errorf("%s：got %s, want %s", tt.name, got, tt.want)
		}
	}
}

// ---------- ReviewService ----------

func TestReviewService_Transitions(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv()
	u := env.register(t, "alice")

	steps := []struct {
		name string
		op   func(context.Context, int64) error
		want error
		then AccountStatus
	}{
		{"待审核 → 拒绝", env.review.Reject, nil, StatusRejected},
		{"已拒绝不能再拒绝", env.review.Reject, ErrInvalidStatusTransition, StatusRejected},
		{"已拒绝 → 批准", env.review.Approve, nil, StatusActive},
		{"已通过不能再批准", env.review.Approve, ErrInvalidStatusTransition, StatusActive},
		{"已通过不能改成拒绝", env.review.Reject, ErrInvalidStatusTransition, StatusActive},
	}
	for _, s := range steps {
		if err := s.op(ctx, u.ID); !errors.Is(err, s.want) {
			t.Errorf("%s：err = %v, want %v", s.name, err, s.want)
		}
		if got, _ := env.repo.FindByID(ctx, u.ID); got.Status != s.then {
			t.Errorf("%s：status = %s, want %s", s.name, got.Status, s.then)
		}
	}

	pending := env.register(t, "bob")
	if err := env.review.Approve(ctx, pending.ID); err != nil {
		t.Errorf("待审核 → 批准：%v", err)
	}
	if err := env.review.Approve(ctx, 1999999999); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("账号不存在：err = %v", err)
	}
}

func TestReviewService_List(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv()
	a := env.register(t, "alice")
	b := env.register(t, "bob")
	c := env.register(t, "carol")
	if err := env.review.Reject(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.review.Approve(ctx, c.ID); err != nil {
		t.Fatal(err)
	}

	ids := func(status AccountStatus) []int64 {
		items, err := env.review.List(ctx, status)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, it := range items {
			out = append(out, it.User.ID)
		}
		return out
	}
	if got := ids(StatusPending); !slices.Equal(got, []int64{a.ID}) {
		t.Errorf("pending = %v, want [%d]", got, a.ID)
	}
	if got := ids(StatusRejected); !slices.Equal(got, []int64{b.ID}) {
		t.Errorf("rejected = %v, want [%d]", got, b.ID)
	}
}
