package user

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// 这里的测试连接真实的 PostgreSQL，验证 fakeRepo 模拟不了的东西：并发验证只有一个能冻结、
// 同一 IP 的并发验证不会突破次数（咨询锁）、并发提交只有一个能用掉邀请码、事务回滚、
// 手写 SQL（Raw）的唯一约束错误也被翻译、INET 列的读写、搜索 SQL 里 OR 的括号。
// 测试库的准备方法见 internal/message/repository_test.go；没设置 TEST_DATABASE_DSN 时跳过。

type dbEnv struct {
	db      *gorm.DB
	repo    Repository
	adminID int64
	net     string // 本次测试专用的 /24 网段（如 10.12.34.），验证记录按它清理
	ctx     context.Context
}

func setupUserDB(t *testing.T) *dbEnv {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("TEST_DATABASE_DSN not set, skipping database test")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		TranslateError: true,
		Logger:         gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	// 随机 ID 段和 IP 网段，避免和库里已有数据、并行运行的其他测试冲突。管理员占 base，测试里的账号用 base+1 起
	base := 1_000_000_000 + rand.Int64N(8_000_000_000)
	netPrefix := fmt.Sprintf("10.%d.%d.", rand.IntN(256), rand.IntN(256))
	if err := db.Exec("INSERT INTO users (id, username, password_hash, status) VALUES (?, 'adm', 'x', 'active')", base).Error; err != nil {
		t.Fatalf("insert admin: %v", err)
	}
	t.Cleanup(func() {
		// 按外键依赖倒序删除。这是测试库，物理删除没问题
		codes := db.Table("invite_codes").Select("id").Where("created_by = ?", base)
		db.Exec("DELETE FROM invite_verifications WHERE invite_code_id IN (?) OR ip << ?::inet", codes, netPrefix+"0/24")
		db.Exec("DELETE FROM users WHERE invite_code_id IN (?)", codes)
		db.Exec("DELETE FROM users WHERE id BETWEEN ? AND ?", base+1, base+1000)
		db.Exec("DELETE FROM invite_codes WHERE created_by = ?", base)
		db.Exec("DELETE FROM users WHERE id = ?", base)
	})
	return &dbEnv{db: db, repo: NewRepository(db), adminID: base, net: netPrefix, ctx: context.Background()}
}

func (e *dbEnv) ip(n int) string { return fmt.Sprintf("%s%d", e.net, n) }

// newInvite 生成一个随机码的邀请码
func (e *dbEnv) newInvite(t *testing.T, ttl time.Duration) *InviteCode {
	t.Helper()
	code, err := RandomInviteCode()
	if err != nil {
		t.Fatal(err)
	}
	c := &InviteCode{Code: code, CreatedBy: e.adminID, Note: "测试"}
	if err := e.repo.CreateInvite(e.ctx, c, ttl); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	return c
}

// reserveReq 用正式的限流参数构造验证请求
func reserveReq(code, ip, tokenHash string) ReserveRequest {
	return ReserveRequest{
		Code: code, IP: ip, TokenHash: tokenHash, Lease: ReservationLease,
		IPPerDay: verifyIPPerDay, CodePerDay: verifyCodePerDay, CodeMinInterval: verifyCodeMinInterval,
	}
}

// reserve 冻结一个码，返回令牌的哈希
func (e *dbEnv) reserve(t *testing.T, code string, ip string) string {
	t.Helper()
	hash := hashToken(fmt.Sprintf("token-%s-%s", code, ip))
	if _, err := e.repo.ReserveInvite(e.ctx, reserveReq(code, ip, hash)); err != nil {
		t.Fatalf("ReserveInvite: %v", err)
	}
	return hash
}

func (e *dbEnv) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if err := e.db.Exec(sql, args...).Error; err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (e *dbEnv) invite(t *testing.T, id int64) InviteCode {
	t.Helper()
	var c InviteCode
	if err := e.db.First(&c, id).Error; err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRepo_CreateInvite(t *testing.T) {
	e := setupUserDB(t)
	c := e.newInvite(t, 72*time.Hour)
	if c.ID == 0 || c.CreatedBy != e.adminID || c.Note != "测试" || c.UsedAt != nil || c.RevokedAt != nil || c.ReservedUntil != nil {
		t.Errorf("RETURNING 回填的记录不对: %+v", c)
	}
	// 同一条语句里的 now() 是同一个值，所以过期时间恰好是 created_at + 72h
	if d := c.ExpiresAt.Sub(c.CreatedAt); d != 72*time.Hour {
		t.Errorf("expires_at - created_at = %s, want 72h", d)
	}

	dup := &InviteCode{Code: c.Code, CreatedBy: e.adminID}
	if err := e.repo.CreateInvite(e.ctx, dup, time.Hour); !errors.Is(err, ErrDuplicateInviteCode) {
		t.Errorf("重复的码：err = %v, want ErrDuplicateInviteCode（Raw 的错误也要被翻译）", err)
	}
	code, _ := RandomInviteCode()
	ghost := &InviteCode{Code: code, CreatedBy: 1999999999}
	if err := e.repo.CreateInvite(e.ctx, ghost, time.Hour); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("创建者不存在：err = %v, want ErrUserNotFound", err)
	}
}

// 10 个人同时验证同一个码：恰好 1 个冻结成功，其余是"使用中"，每次尝试都有记录
func TestRepo_ReserveInvite_Concurrent(t *testing.T) {
	e := setupUserDB(t)
	c := e.newInvite(t, time.Hour)

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			_, errs[i] = e.repo.ReserveInvite(e.ctx, reserveReq(c.Code, e.ip(i+1), hashToken(fmt.Sprint(i))))
		})
	}
	wg.Wait()

	ok, inUse := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrInviteInUse):
			inUse++
		default:
			t.Errorf("意外的错误: %v", err)
		}
	}
	if ok != 1 || inUse != n-1 {
		t.Errorf("冻结成功 %d 个、使用中 %d 个，want 1 和 %d", ok, inUse, n-1)
	}
	list, err := e.repo.ListVerifications(e.ctx, []int64{c.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != n {
		t.Errorf("验证记录 %d 条，want %d", len(list), n)
	}
}

// 同一个 IP 并发发 15 个验证请求：咨询锁让它们串行，恰好 10 个被处理，5 个超出 IP 限制
func TestRepo_ReserveInvite_IPLimitConcurrent(t *testing.T) {
	e := setupUserDB(t)
	ip := e.ip(1)

	const n = 15
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			_, errs[i] = e.repo.ReserveInvite(e.ctx, reserveReq(fmt.Sprintf("ZZZZ%02d", i), ip, "x"))
		})
	}
	wg.Wait()

	invalid, limited := 0, 0
	for _, err := range errs {
		switch {
		case errors.Is(err, ErrInvalidInvite):
			invalid++
		case errors.Is(err, ErrVerifyIPLimit):
			limited++
		default:
			t.Errorf("意外的错误: %v", err)
		}
	}
	if invalid != 10 || limited != 5 {
		t.Errorf("无效 %d 个、超出 IP 限制 %d 个，want 10 和 5", invalid, limited)
	}
	var recorded int64
	e.db.Raw("SELECT count(*) FROM invite_verifications WHERE ip = ?", ip).Scan(&recorded)
	if recorded != 10 {
		t.Errorf("记录了 %d 条，want 10（超出 IP 限制的不记录）", recorded)
	}

	// 24 小时前的记录不算：把它们挪到 25 小时前，又可以验证了
	e.exec(t, "UPDATE invite_verifications SET created_at = now() - interval '25 hours' WHERE ip = ?", ip)
	if _, err := e.repo.ReserveInvite(e.ctx, reserveReq("ZZZZZZ", ip, "x")); !errors.Is(err, ErrInvalidInvite) {
		t.Errorf("24 小时后：err = %v", err)
	}
}

func TestRepo_ReserveInvite_CodeLimits(t *testing.T) {
	e := setupUserDB(t)
	c := e.newInvite(t, time.Hour)

	hash := e.reserve(t, c.Code, e.ip(1))
	if err := e.repo.ReleaseReservation(e.ctx, c.Code, hash, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.repo.ReserveInvite(e.ctx, reserveReq(c.Code, e.ip(2), "h2")); !errors.Is(err, ErrInviteVerifyTooFrequent) {
		t.Fatalf("1 分钟内再验证：err = %v", err)
	}

	// 模拟已经在 24 小时内验证成功了 10 次（都在 1 分钟以前）
	e.exec(t, "UPDATE invite_verifications SET created_at = now() - interval '2 minutes' WHERE invite_code_id = ?", c.ID)
	for range verifyCodePerDay - 1 {
		e.exec(t, "INSERT INTO invite_verifications (ip, invite_code_id, result, created_at) VALUES (?, ?, 'reserved', now() - interval '1 hour')", e.ip(3), c.ID)
	}
	if _, err := e.repo.ReserveInvite(e.ctx, reserveReq(c.Code, e.ip(4), "h4")); !errors.Is(err, ErrInviteVerifyDailyLimit) {
		t.Fatalf("24 小时内第 11 次：err = %v", err)
	}
	e.exec(t, "UPDATE invite_verifications SET created_at = now() - interval '25 hours' WHERE invite_code_id = ?", c.ID)
	if _, err := e.repo.ReserveInvite(e.ctx, reserveReq(c.Code, e.ip(5), "h5")); err != nil {
		t.Errorf("24 小时后：%v", err)
	}

	// IPv4 / IPv6 都能存，读出来不带掩码
	if _, err := e.repo.ReserveInvite(e.ctx, reserveReq(c.Code, "2001:db8::7", "h6")); !errors.Is(err, ErrInviteInUse) {
		t.Fatal(err)
	}
	e.exec(t, "DELETE FROM invite_verifications WHERE ip = '2001:db8::7'::inet AND invite_code_id = ?", c.ID)
	list, err := e.repo.ListVerifications(e.ctx, []int64{c.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 || list[0].IP != e.ip(5) || list[0].Result != VerifyReserved {
		t.Errorf("最新的验证记录 = %+v, want %s reserved", list[0], e.ip(5))
	}
}

// 10 个请求拿着同一个令牌同时提交：恰好 1 个注册成功，邀请码只被用一次
func TestRepo_CreateWithInvite_Concurrent(t *testing.T) {
	e := setupUserDB(t)
	c := e.newInvite(t, time.Hour)
	hash := e.reserve(t, c.Code, e.ip(1))

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			u := &User{ID: e.adminID + 1 + int64(i), Username: fmt.Sprintf("c%d", i), PasswordHash: "x", Status: StatusPending}
			errs[i] = e.repo.CreateWithInvite(e.ctx, u, c.Code, hash)
		})
	}
	wg.Wait()

	ok, lost := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrReservationLost):
			lost++
		default:
			t.Errorf("意外的错误: %v", err)
		}
	}
	if ok != 1 || lost != n-1 {
		t.Errorf("成功 %d 个、冻结失效 %d 个，want 1 和 %d", ok, lost, n-1)
	}
	got := e.invite(t, c.ID)
	if got.UsedAt == nil || got.ReservedUntil != nil || got.ReservationHash != nil {
		t.Errorf("使用后应写入 used_at、清空冻结: %+v", got)
	}
}

// 插入用户失败（ID 冲突）时整个事务回滚，邀请码仍是冻结状态；成功时回填 status、invite_code_id、created_at
func TestRepo_CreateWithInvite_Rollback(t *testing.T) {
	e := setupUserDB(t)
	c := e.newInvite(t, time.Hour)
	hash := e.reserve(t, c.Code, e.ip(1))

	taken := &User{ID: e.adminID, Username: "dup", PasswordHash: "x", Status: StatusPending} // 和管理员 ID 冲突
	if err := e.repo.CreateWithInvite(e.ctx, taken, c.Code, hash); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("err = %v, want ErrDuplicateID", err)
	}
	if got := e.invite(t, c.ID); got.UsedAt != nil || got.ReservationHash == nil {
		t.Fatalf("ID 冲突后应回滚，码仍冻结: %+v", got)
	}

	u := &User{ID: e.adminID + 1, Username: "ok", PasswordHash: "x", Status: StatusPending}
	if err := e.repo.CreateWithInvite(e.ctx, u, c.Code, hash); err != nil {
		t.Fatalf("重试：%v", err)
	}
	got, err := e.repo.FindByID(e.ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPending || got.InviteCodeID == nil || *got.InviteCodeID != c.ID || !got.CreatedAt.Equal(u.CreatedAt) {
		t.Errorf("库里的账号 = %+v, 插入时回填 = %+v", got, u)
	}

	// 数据库兜底：一个码最多对应一个账号
	err = e.db.Exec("INSERT INTO users (id, username, password_hash, invite_code_id) VALUES (?, 'x', 'x', ?)", e.adminID+2, c.ID).Error
	if !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Errorf("同一个码第二个账号：err = %v, want 唯一约束冲突", err)
	}
}

func TestRepo_Reservation(t *testing.T) {
	e := setupUserDB(t)

	t.Run("续期、最长冻结时间", func(t *testing.T) {
		c := e.newInvite(t, time.Hour)
		hash := e.reserve(t, c.Code, e.ip(1))
		got, err := e.repo.RenewReservation(e.ctx, c.Code, hash, ReservationLease, ReservationMaxHold)
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Until(*got.ReservedUntil); d < ReservationLease-10*time.Second || d > ReservationLease+10*time.Second {
			t.Errorf("续期后还剩 %s, want 约 %s", d, ReservationLease)
		}
		if !got.ExpiresAt.Equal(c.ExpiresAt) {
			t.Error("续期不应改变有效期")
		}
		if _, err := e.repo.RenewReservation(e.ctx, c.Code, "wrong", ReservationLease, ReservationMaxHold); !errors.Is(err, ErrReservationLost) {
			t.Errorf("令牌不对：err = %v", err)
		}
		e.exec(t, "UPDATE invite_codes SET reserved_at = now() - interval '31 minutes' WHERE id = ?", c.ID)
		if _, err := e.repo.RenewReservation(e.ctx, c.Code, hash, ReservationLease, ReservationMaxHold); !errors.Is(err, ErrReservationLost) {
			t.Errorf("超过 30 分钟：err = %v", err)
		}
	})

	t.Run("软释放、立即释放", func(t *testing.T) {
		c := e.newInvite(t, time.Hour)
		hash := e.reserve(t, c.Code, e.ip(2))
		if err := e.repo.ReleaseReservation(e.ctx, c.Code, hash, ReservationGrace); err != nil {
			t.Fatal(err)
		}
		got := e.invite(t, c.ID)
		if d := time.Until(*got.ReservedUntil); d > ReservationGrace+5*time.Second || d <= 0 {
			t.Errorf("软释放后还剩 %s, want 约 %s", d, ReservationGrace)
		}
		if err := e.repo.CheckReservation(e.ctx, c.Code, hash); err != nil {
			t.Errorf("软释放期间冻结仍有效：%v", err)
		}
		if err := e.repo.ReleaseReservation(e.ctx, c.Code, hash, 0); err != nil {
			t.Fatal(err)
		}
		if got := e.invite(t, c.ID); got.ReservedAt != nil || got.ReservedUntil != nil || got.ReservationHash != nil {
			t.Errorf("立即释放应清空冻结: %+v", got)
		}
		if err := e.repo.CheckReservation(e.ctx, c.Code, hash); !errors.Is(err, ErrReservationLost) {
			t.Errorf("释放后：err = %v", err)
		}
	})

	t.Run("租约到期：别人可以验证，原持有者不能提交", func(t *testing.T) {
		c := e.newInvite(t, time.Hour)
		hash := e.reserve(t, c.Code, e.ip(3))
		e.exec(t, "UPDATE invite_codes SET reserved_until = now() - interval '1 second' WHERE id = ?", c.ID)
		e.exec(t, "UPDATE invite_verifications SET created_at = now() - interval '2 minutes' WHERE invite_code_id = ?", c.ID)
		u := &User{ID: e.adminID + 10, Username: "late", PasswordHash: "x", Status: StatusPending}
		if err := e.repo.CreateWithInvite(e.ctx, u, c.Code, hash); !errors.Is(err, ErrReservationLost) {
			t.Errorf("到期后提交：err = %v", err)
		}
		if _, err := e.repo.ReserveInvite(e.ctx, reserveReq(c.Code, e.ip(4), "other")); err != nil {
			t.Errorf("到期后别人验证：%v", err)
		}
	})

	t.Run("过期、作废的码不能验证", func(t *testing.T) {
		expired := e.newInvite(t, time.Hour)
		e.exec(t, "UPDATE invite_codes SET expires_at = now() - interval '1 second' WHERE id = ?", expired.ID)
		if _, err := e.repo.ReserveInvite(e.ctx, reserveReq(expired.Code, e.ip(5), "h")); !errors.Is(err, ErrInvalidInvite) {
			t.Errorf("过期：err = %v", err)
		}
		revoked := e.newInvite(t, time.Hour)
		hash := e.reserve(t, revoked.Code, e.ip(6))
		got, err := e.repo.RevokeInvite(e.ctx, revoked.ID, e.adminID)
		if err != nil || got.RevokedAt == nil || got.ReservationHash != nil {
			t.Fatalf("作废冻结中的码：%+v, %v", got, err)
		}
		if err := e.repo.CheckReservation(e.ctx, revoked.Code, hash); !errors.Is(err, ErrReservationLost) {
			t.Errorf("作废后原持有者：err = %v", err)
		}
		again, err := e.repo.RevokeInvite(e.ctx, revoked.ID, e.adminID)
		if err != nil || !again.RevokedAt.Equal(*got.RevokedAt) {
			t.Errorf("重复作废应原样返回：%+v, %v", again, err)
		}
		if _, err := e.repo.RevokeInvite(e.ctx, revoked.ID, e.adminID+1); !errors.Is(err, ErrInviteNotFound) {
			t.Errorf("别人的码：err = %v", err)
		}
	})
}

// 搜索只返回已通过的账号。OR 两边少了括号时，按用户名能搜到待审核的账号，这个测试能发现
func TestRepo_Search_OnlyActive(t *testing.T) {
	e := setupUserDB(t)
	name := fmt.Sprintf("s%d", e.adminID) // 随机用户名，不和库里其他数据撞上
	for i, status := range []AccountStatus{StatusActive, StatusPending, StatusRejected} {
		e.exec(t, "INSERT INTO users (id, username, password_hash, status) VALUES (?, ?, 'x', ?)", e.adminID+1+int64(i), name, status)
	}
	for _, id := range []int64{0, e.adminID + 2, e.adminID + 3} {
		users, err := e.repo.SearchByUsernameOrID(e.ctx, name, id, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(users) != 1 || users[0].ID != e.adminID+1 {
			t.Errorf("按用户名 + ID %d 搜索 = %+v, want 只有已通过的 %d", id, users, e.adminID+1)
		}
	}
}

func TestRepo_UpdateStatus(t *testing.T) {
	e := setupUserDB(t)
	id := e.adminID + 1
	e.exec(t, "INSERT INTO users (id, username, password_hash) VALUES (?, 'p', 'x')", id)
	u, _ := e.repo.FindByID(e.ctx, id)
	if u.Status != StatusPending {
		t.Fatalf("不写 status 时数据库默认值应为 pending，got %q", u.Status)
	}
	if ok, err := e.repo.UpdateStatus(e.ctx, id, StatusActive, StatusRejected); ok || err != nil {
		t.Errorf("当前是 pending，from=[rejected] 不应生效：%v %v", ok, err)
	}
	if ok, err := e.repo.UpdateStatus(e.ctx, id, StatusActive, StatusPending, StatusRejected); !ok || err != nil {
		t.Errorf("pending → active 应生效：%v %v", ok, err)
	}
	users, err := e.repo.ListByStatus(e.ctx, StatusActive, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range users {
		found = found || u.ID == id
		if u.PasswordHash != "" {
			t.Errorf("ListByStatus 不应读 password_hash")
		}
	}
	if !found {
		t.Errorf("ListByStatus(active) 里没有 %d", id)
	}
}
