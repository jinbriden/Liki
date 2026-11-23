package user

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRepo 用 map 模拟 users、invite_codes、invite_verifications 三张表：用户 ID 是主键（重复时返回 ErrDuplicateID），
// 用户名可以重复；邀请码的 code 唯一。所有方法持有同一把锁，多步操作因此是原子的，等价于真实实现的事务和行锁。
// 时间用 now()：时钟是冻结的，只有 advance 会让它往前走，测试里可以精确模拟"过了 1 分钟""过了 24 小时"
type fakeRepo struct {
	mu            sync.Mutex
	users         map[int64]*User
	invites       map[int64]*InviteCode
	verifications []InviteVerification
	nextInv       int64
	now           func() time.Time
}

func newFakeRepo() *fakeRepo {
	start := time.Now().Truncate(time.Microsecond)
	return &fakeRepo{
		users:   map[int64]*User{},
		invites: map[int64]*InviteCode{},
		now:     func() time.Time { return start },
	}
}

// advance 把 fakeRepo 的时钟往后拨 d
func (r *fakeRepo) advance(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.now().Add(d)
	r.now = func() time.Time { return t }
}

// usable 与真实实现的 inviteUsable 条件一致
func (r *fakeRepo) usable(c *InviteCode) bool {
	return c.UsedAt == nil && c.RevokedAt == nil && r.now().Before(c.ExpiresAt)
}

func (r *fakeRepo) held(c *InviteCode) bool {
	return c.ReservedUntil != nil && r.now().Before(*c.ReservedUntil)
}

// heldByToken 与真实实现的 heldByToken 条件一致
func (r *fakeRepo) heldByToken(c *InviteCode, tokenHash string) bool {
	return c != nil && c.ReservationHash != nil && *c.ReservationHash == tokenHash && r.held(c) && r.usable(c)
}

func (r *fakeRepo) inviteByCode(code string) *InviteCode {
	for _, c := range r.invites {
		if c.Code == code {
			return c
		}
	}
	return nil
}

func clearReservation(c *InviteCode) {
	c.ReservedAt, c.ReservedUntil, c.ReservationHash = nil, nil, nil
}

func ptr[T any](v T) *T { return &v }

func (r *fakeRepo) CreateWithInvite(ctx context.Context, u *User, code, tokenHash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.inviteByCode(code)
	if !r.heldByToken(c, tokenHash) {
		return ErrReservationLost
	}
	// 真实实现里先使用邀请码、插入冲突时整体回滚；这里先检查冲突再使用，效果相同
	if _, ok := r.users[u.ID]; ok {
		return ErrDuplicateID
	}
	c.UsedAt = ptr(r.now())
	clearReservation(c)
	u.InviteCodeID = ptr(c.ID)
	if u.Status == "" {
		u.Status = StatusPending // 模拟数据库默认值
	}
	u.CreatedAt = r.now() // 模拟数据库 now() 回填，精度微秒
	cp := *u              // 存副本，避免调用方之后修改 u 影响"库里"的数据
	r.users[u.ID] = &cp
	return nil
}

// addUser 直接往"库里"放一个账号（绕过注册流程），用于准备测试数据
func (r *fakeRepo) addUser(u User) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u.Status == "" {
		u.Status = StatusActive
	}
	u.CreatedAt = r.now()
	r.users[u.ID] = &u
}

// setStatus 直接修改账号状态，用于准备测试数据
func (r *fakeRepo) setStatus(id int64, s AccountStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[id].Status = s
}

// invite 返回某个邀请码当前的副本，用于断言
func (r *fakeRepo) invite(id int64) InviteCode {
	r.mu.Lock()
	defer r.mu.Unlock()
	return *r.invites[id]
}

// editInvite 直接修改邀请码（模拟过期等），用于准备测试数据
func (r *fakeRepo) editInvite(id int64, fn func(*InviteCode)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r.invites[id])
}

func (r *fakeRepo) FindByID(ctx context.Context, id int64) (*User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[id]
	if !ok {
		return nil, ErrUserNotFound
	}
	cp := *u
	return &cp, nil
}

func (r *fakeRepo) FindByIDs(ctx context.Context, ids []int64) ([]User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []User
	for _, id := range ids {
		if u, ok := r.users[id]; ok {
			out = append(out, *u)
		}
	}
	return out, nil
}

// SearchByUsernameOrID 用户名只含 ASCII，strings.EqualFold 与 SQL 的 lower() 比较结果一致
func (r *fakeRepo) SearchByUsernameOrID(ctx context.Context, username string, id int64, limit int) ([]User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []User
	for _, u := range r.users {
		if (strings.EqualFold(u.Username, username) || u.ID == id) && u.Status == StatusActive {
			out = append(out, *u)
		}
	}
	// 与真实实现一致：ID 命中的排最前，其余按 ID 升序
	slices.SortFunc(out, func(a, b User) int {
		if (a.ID == id) != (b.ID == id) {
			if a.ID == id {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// sortedUsers 按注册时间、ID 排序（与真实实现的 ORDER BY created_at, id 一致）
func sortedUsers(out []User) []User {
	slices.SortFunc(out, func(a, b User) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out
}

func (r *fakeRepo) ListByStatus(ctx context.Context, status AccountStatus, limit int) ([]User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []User
	for _, u := range r.users {
		if u.Status == status {
			out = append(out, *u)
		}
	}
	out = sortedUsers(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *fakeRepo) UpdateStatus(ctx context.Context, id int64, to AccountStatus, from ...AccountStatus) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[id]
	if !ok || !slices.Contains(from, u.Status) {
		return false, nil
	}
	u.Status = to
	return true, nil
}

// ReserveInvite 按真实实现的顺序检查：IP 次数 → 码是否有效 → 是否被冻结 → 码的验证频率 → 冻结
func (r *fakeRepo) ReserveInvite(ctx context.Context, req ReserveRequest) (*InviteCode, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	dayAgo := now.Add(-24 * time.Hour)

	ipCount := 0
	for _, v := range r.verifications {
		if v.IP == req.IP && v.CreatedAt.After(dayAgo) {
			ipCount++
		}
	}
	if ipCount >= req.IPPerDay {
		return nil, ErrVerifyIPLimit
	}
	record := func(inviteID *int64, result VerifyResult) {
		r.verifications = append(r.verifications, InviteVerification{
			ID: int64(len(r.verifications) + 1), IP: req.IP, InviteCodeID: inviteID, Result: result, CreatedAt: now,
		})
	}

	c := r.inviteByCode(req.Code)
	switch {
	case c == nil:
		record(nil, VerifyInvalid)
		return nil, ErrInvalidInvite
	case !r.usable(c):
		record(ptr(c.ID), VerifyInvalid)
		return nil, ErrInvalidInvite
	case r.held(c):
		record(ptr(c.ID), VerifyInUse)
		return nil, ErrInviteInUse
	}

	day, recent := 0, 0
	for _, v := range r.verifications {
		if v.InviteCodeID == nil || *v.InviteCodeID != c.ID || v.Result != VerifyReserved {
			continue
		}
		if v.CreatedAt.After(dayAgo) {
			day++
		}
		if v.CreatedAt.After(now.Add(-req.CodeMinInterval)) {
			recent++
		}
	}
	switch {
	case recent > 0:
		record(ptr(c.ID), VerifyLimited)
		return nil, ErrInviteVerifyTooFrequent
	case day >= req.CodePerDay:
		record(ptr(c.ID), VerifyLimited)
		return nil, ErrInviteVerifyDailyLimit
	}

	c.ReservedAt, c.ReservedUntil, c.ReservationHash = ptr(now), ptr(now.Add(req.Lease)), ptr(req.TokenHash)
	record(ptr(c.ID), VerifyReserved)
	cp := *c
	return &cp, nil
}

func (r *fakeRepo) CheckReservation(ctx context.Context, code, tokenHash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.heldByToken(r.inviteByCode(code), tokenHash) {
		return ErrReservationLost
	}
	return nil
}

func (r *fakeRepo) RenewReservation(ctx context.Context, code, tokenHash string, lease, maxHold time.Duration) (*InviteCode, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.inviteByCode(code)
	now := r.now()
	if !r.heldByToken(c, tokenHash) || !c.ReservedAt.Add(maxHold).After(now) {
		return nil, ErrReservationLost
	}
	until := now.Add(lease)
	if limit := c.ReservedAt.Add(maxHold); limit.Before(until) {
		until = limit
	}
	c.ReservedUntil = &until
	cp := *c
	return &cp, nil
}

func (r *fakeRepo) ReleaseReservation(ctx context.Context, code, tokenHash string, grace time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.inviteByCode(code)
	if c == nil || c.ReservationHash == nil || *c.ReservationHash != tokenHash || c.UsedAt != nil {
		return nil
	}
	if grace <= 0 {
		clearReservation(c)
		return nil
	}
	if r.held(c) {
		if limit := r.now().Add(grace); limit.Before(*c.ReservedUntil) {
			c.ReservedUntil = &limit
		}
	}
	return nil
}

func (r *fakeRepo) CreateInvite(ctx context.Context, c *InviteCode, ttl time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inviteByCode(c.Code) != nil {
		return ErrDuplicateInviteCode
	}
	if _, ok := r.users[c.CreatedBy]; !ok {
		return ErrUserNotFound // 模拟外键约束
	}
	r.nextInv++
	now := r.now()
	c.ID, c.CreatedAt, c.ExpiresAt = r.nextInv, now, now.Add(ttl)
	cp := *c
	r.invites[c.ID] = &cp
	return nil
}

func (r *fakeRepo) FindInvite(ctx context.Context, id, creatorID int64) (*InviteCode, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.invites[id]
	if !ok || c.CreatedBy != creatorID {
		return nil, ErrInviteNotFound
	}
	cp := *c
	return &cp, nil
}

func (r *fakeRepo) FindInvitesByIDs(ctx context.Context, ids []int64) ([]InviteCode, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []InviteCode
	for _, id := range ids {
		if c, ok := r.invites[id]; ok {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (r *fakeRepo) ListInvitesByCreator(ctx context.Context, creatorID int64, limit int) ([]InviteCode, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []InviteCode
	for _, c := range r.invites {
		if c.CreatedBy == creatorID {
			out = append(out, *c)
		}
	}
	// 最新的在前；同一微秒内生成的按 ID 倒序（与真实实现的 ORDER BY created_at DESC, id DESC 一致）
	slices.SortFunc(out, func(a, b InviteCode) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), cmp.Compare(b.ID, a.ID))
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *fakeRepo) RevokeInvite(ctx context.Context, id, creatorID int64) (*InviteCode, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.invites[id]
	if !ok || c.CreatedBy != creatorID {
		return nil, ErrInviteNotFound
	}
	if c.RevokedAt == nil && c.UsedAt == nil {
		c.RevokedAt = ptr(r.now())
		clearReservation(c)
	}
	cp := *c
	return &cp, nil
}

func (r *fakeRepo) ListUsersByInvites(ctx context.Context, inviteIDs []int64) ([]User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []User
	for _, u := range r.users {
		if u.InviteCodeID != nil && slices.Contains(inviteIDs, *u.InviteCodeID) {
			cp := *u
			cp.PasswordHash = "" // 真实实现不读这一列
			out = append(out, cp)
		}
	}
	return sortedUsers(out), nil
}

func (r *fakeRepo) ListVerifications(ctx context.Context, inviteIDs []int64) ([]InviteVerification, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []InviteVerification
	for _, v := range slices.Backward(r.verifications) { // 最新的在前
		if v.InviteCodeID != nil && slices.Contains(inviteIDs, *v.InviteCodeID) {
			out = append(out, v)
		}
	}
	return out, nil
}

// counterIDs 从 1000000001 开始依次递增生成 ID
func counterIDs() IDGenerator {
	var mu sync.Mutex
	next := int64(minUserID)
	return func() (int64, error) {
		mu.Lock()
		defer mu.Unlock()
		next++
		return next, nil
	}
}

// fixedIDs 按顺序返回给定的 ID，用完后返回错误
func fixedIDs(ids ...int64) IDGenerator {
	i := 0
	return func() (int64, error) {
		if i >= len(ids) {
			return 0, errors.New("fixedIDs: exhausted")
		}
		i++
		return ids[i-1], nil
	}
}

// fakeTokens 返回可预测的假令牌；err 不为空时模拟签发失败
type fakeTokens struct{ err error }

func (f fakeTokens) Issue(userID int64) (string, time.Time, error) {
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	return fmt.Sprintf("token-for-%d", userID), time.Unix(1_800_000_000, 0), nil
}

// testAdminID 测试里的管理员账号。ID 取在 counterIDs 生成范围之外，不会和注册的账号冲突
const testAdminID int64 = 9_000_000_001

// counterCodes 依次生成 A00001、A00002……，测试里可以预知
func counterCodes() CodeGenerator {
	var mu sync.Mutex
	n := 0
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		return fmt.Sprintf("A%05d", n), nil
	}
}

// fixedCodes 按顺序返回给定的码，用完后返回错误
func fixedCodes(codes ...string) CodeGenerator {
	i := 0
	return func() (string, error) {
		if i >= len(codes) {
			return "", errors.New("fixedCodes: exhausted")
		}
		i++
		return codes[i-1], nil
	}
}

// newTestRepo 带一个已通过的管理员账号的 fakeRepo
func newTestRepo() *fakeRepo {
	repo := newFakeRepo()
	repo.addUser(User{ID: testAdminID, Username: "adm", Status: StatusActive})
	return repo
}

// testEnv 测试用的一组 service，共用同一个 fakeRepo
type testEnv struct {
	repo    *fakeRepo
	svc     *Service
	invites *InviteService
	review  *ReviewService
	ipSeq   int
}

func newTestEnv(opts ...Option) *testEnv {
	repo := newTestRepo()
	return &testEnv{
		repo:    repo,
		svc:     NewService(repo, fakeTokens{}, counterIDs(), opts...),
		invites: NewInviteService(repo, testAdminID, counterCodes()),
		review:  NewReviewService(repo),
	}
}

// nextIP 每次返回一个新的 IP，避免准备数据时撞上"每个 IP 每天验证 10 次"的限制
func (e *testEnv) nextIP() string {
	e.ipSeq++
	return fmt.Sprintf("198.51.%d.%d", e.ipSeq/250, e.ipSeq%250+1)
}

// newInvite 以管理员名义生成一个邀请码
func (e *testEnv) newInvite(t *testing.T, opts InviteOptions) *InviteCode {
	t.Helper()
	c, err := e.invites.Create(context.Background(), testAdminID, opts)
	if err != nil {
		t.Fatalf("准备邀请码失败: %v", err)
	}
	return c
}

// reserve 验证（冻结）一个邀请码，返回冻结令牌
func (e *testEnv) reserve(t *testing.T, code string) string {
	t.Helper()
	r, err := e.invites.Verify(context.Background(), code, e.nextIP())
	if err != nil {
		t.Fatalf("验证邀请码失败: %v", err)
	}
	return r.Token
}

// register 生成邀请码 → 验证 → 提交注册申请（账号为待审核）
func (e *testEnv) register(t *testing.T, username string) *User {
	t.Helper()
	code := e.newInvite(t, InviteOptions{}).Code
	u, err := e.svc.Register(context.Background(), username, validPassword, code, e.reserve(t, code))
	if err != nil {
		t.Fatalf("Register(%q): %v", username, err)
	}
	return u
}

// registerActive 注册并批准，得到一个能登录的账号
func (e *testEnv) registerActive(t *testing.T, username string) *User {
	t.Helper()
	u := e.register(t, username)
	if err := e.review.Approve(context.Background(), u.ID); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	u.Status = StatusActive
	return u
}
