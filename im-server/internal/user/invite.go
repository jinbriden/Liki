package user

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"
)

// 邀请码规则（FR-23）
const (
	inviteCodeLen      = 6
	inviteCodeAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	maxCodeAttempts    = 5 // 生成的码与已有的码重复时的最大尝试次数

	DefaultInviteTTL = 72 * time.Hour
	// 命令行可以指定有效期，但限制在合理范围内，防止手误生成一个一年后才过期的码
	MinInviteTTL     = time.Hour
	MaxInviteTTL     = 30 * 24 * time.Hour
	maxInviteNoteLen = 50 // 按字符计数，和数据库 VARCHAR(50) 一致

	inviteListLimit = 100 // 列表只返回最近的这么多个
)

// 冻结（验证通过、正在填写注册表单）的租约参数。
// 浏览器崩溃、断网时服务端收不到"离开"的通知，所以冻结必须会自己到期：页面开着时每 30 秒续期一次，
// 租约 3 分钟能扛住浏览器对后台标签页定时器的节流（最慢约 1 分钟执行一次）
const (
	ReservationLease   = 3 * time.Minute
	ReservationMaxHold = 30 * time.Minute // 从验证时算起最多冻结这么久，之后要重新验证
	// ReservationGrace 软释放后保留的时间。页面卸载时分不清是刷新还是关闭：
	// 刷新的话新页面马上续期，冻结恢复；关闭的话这么久之后码恢复可用
	ReservationGrace = 15 * time.Second
)

// 验证频率（FR-23、NFR-05），计数存在数据库的 invite_verifications 里，重启不清零
const (
	verifyIPPerDay        = 10          // 每个 IP 24 小时内最多验证 10 次（不论码是否有效，防止暴力猜码）
	verifyCodePerDay      = 10          // 每个码 24 小时内最多验证成功 10 次
	verifyCodeMinInterval = time.Minute // 每个码 1 分钟内最多验证成功 1 次
)

// CodeGenerator 生成新的邀请码。抽成函数类型，测试时可以注入固定的码序列
type CodeGenerator func() (string, error)

// RandomInviteCode 用 crypto/rand 生成 6 位邀请码。
// 每一位用 rand.Int 在 [0, 36) 里均匀取值：不能用"随机字节 % 36"，
// 256 不是 36 的整数倍，那样 0-3 对应的字符出现的概率会偏高（取模偏差）。
func RandomInviteCode() (string, error) {
	b := make([]byte, inviteCodeLen)
	limit := big.NewInt(int64(len(inviteCodeAlphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		b[i] = inviteCodeAlphabet[n.Int64()]
	}
	return string(b), nil
}

// NormalizeInviteCode 用户输入的邀请码：去掉首尾空白、转成大写（输入不区分大小写），
// 再检查格式。格式不对的直接拒绝，不用查库
func NormalizeInviteCode(s string) (string, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) != inviteCodeLen {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(inviteCodeAlphabet, rune(s[i])) {
			return "", false
		}
	}
	return s, true
}

// hashToken 冻结令牌的 SHA-256（十六进制）。令牌是 128 位随机数，熵足够高，不需要加盐的慢哈希
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// InviteService 邀请码的生成、查看、作废，以及注册前的验证和冻结（FR-23）。网页接口和命令行工具共用。
// 不放进 Service：命令行工具用不到 JWT，单独一个类型就不必为了它配置 JWT_SECRET
// （类比 Spring 里单独一个 InviteService bean，只注入 Repository）。
type InviteService struct {
	repo    Repository
	adminID int64 // 管理员的用户 ID，0 表示没有配置管理员
	newCode CodeGenerator
}

func NewInviteService(repo Repository, adminID int64, newCode CodeGenerator) *InviteService {
	return &InviteService{repo: repo, adminID: adminID, newCode: newCode}
}

// AdminID 配置的管理员用户 ID，0 表示没有配置
func (s *InviteService) AdminID() int64 { return s.adminID }

// CanInvite 判断某个账号能否生成邀请码。现阶段只有管理员能生成；
// 以后开放给普通用户时只改这里（比如"所有已通过的账号"或"每人限 N 个"），接口和前端都不用改
func (s *InviteService) CanInvite(userID int64) bool {
	return s.adminID != 0 && userID == s.adminID
}

// VerifyAdmin 启动时检查配置的管理员账号存在并且已通过审核，配错了就拒绝启动，
// 免得上线后才发现生成不了邀请码，或者邀请码记在了一个不存在的账号名下
func (s *InviteService) VerifyAdmin(ctx context.Context) error {
	if s.adminID == 0 {
		return errors.New("admin user id not configured")
	}
	u, err := s.repo.FindByID(ctx, s.adminID)
	if err != nil {
		return fmt.Errorf("admin user %d: %w", s.adminID, err)
	}
	if u.Status != StatusActive {
		return fmt.Errorf("admin user %d is %s, want %s", s.adminID, u.Status, StatusActive)
	}
	return nil
}

// ---------- 验证与冻结（注册页使用，无需登录） ----------

// Reservation 验证通过后的冻结。Token 只在验证成功时返回这一次，之后续期、提交注册都要带上它
type Reservation struct {
	Token         string
	ExpiresAt     time.Time // 邀请码本身的过期时间，冻结不会延长它
	ReservedUntil time.Time // 冻结到什么时候（之后靠续期）
}

// Verify 验证邀请码，有效就冻结它（FR-23）：冻结期间别人验证会得到 ErrInviteInUse，
// 只有拿到令牌的页面能提交注册。ip 用于按 IP 限流和溯源
func (s *InviteService) Verify(ctx context.Context, code, ip string) (*Reservation, error) {
	code, ok := NormalizeInviteCode(code)
	if !ok {
		// 格式不对的不查库、不记录：这样的请求猜不中任何码
		slog.InfoContext(ctx, "invite verify rejected", "reason", "malformed", "ip", ip) // 不记录邀请码本身
		return nil, ErrInvalidInvite
	}
	// rand.Text：crypto/rand 生成的 26 位 base32 字符串，128 位随机性
	token := rand.Text()
	inv, err := s.repo.ReserveInvite(ctx, ReserveRequest{
		Code:            code,
		IP:              ip,
		TokenHash:       hashToken(token),
		Lease:           ReservationLease,
		IPPerDay:        verifyIPPerDay,
		CodePerDay:      verifyCodePerDay,
		CodeMinInterval: verifyCodeMinInterval,
	})
	if err != nil {
		reason := verifyRejectReason(err)
		if reason == "" {
			return nil, err // 数据库错误
		}
		level := slog.LevelInfo
		if errors.Is(err, ErrVerifyIPLimit) {
			level = slog.LevelWarn // 一个 IP 一天验证 10 次以上，可能在猜码
		}
		slog.Log(ctx, level, "invite verify rejected", "reason", reason, "ip", ip)
		return nil, err
	}
	slog.InfoContext(ctx, "invite reserved", "invite_id", inv.ID, "ip", ip)
	return &Reservation{Token: token, ExpiresAt: inv.ExpiresAt, ReservedUntil: *inv.ReservedUntil}, nil
}

func verifyRejectReason(err error) string {
	switch {
	case errors.Is(err, ErrVerifyIPLimit):
		return "ip_daily_limit"
	case errors.Is(err, ErrInvalidInvite):
		return "invalid"
	case errors.Is(err, ErrInviteInUse):
		return "in_use"
	case errors.Is(err, ErrInviteVerifyTooFrequent):
		return "code_too_frequent"
	case errors.Is(err, ErrInviteVerifyDailyLimit):
		return "code_daily_limit"
	}
	return ""
}

// Renew 续期冻结（注册页开着时定期调用）。冻结已失效 → ErrReservationLost，页面应回到验证邀请码那一步
func (s *InviteService) Renew(ctx context.Context, code, token string) (*Reservation, error) {
	code, ok := NormalizeInviteCode(code)
	if !ok || token == "" {
		return nil, ErrReservationLost
	}
	inv, err := s.repo.RenewReservation(ctx, code, hashToken(token), ReservationLease, ReservationMaxHold)
	if err != nil {
		return nil, err
	}
	return &Reservation{Token: token, ExpiresAt: inv.ExpiresAt, ReservedUntil: *inv.ReservedUntil}, nil
}

// Release 解冻（离开注册页时调用），码恢复可用，有效期不变。
// soft 为 true 时只保留 ReservationGrace（页面卸载时用：分不清是刷新还是关闭）。令牌不对时什么也不做
func (s *InviteService) Release(ctx context.Context, code, token string, soft bool) error {
	code, ok := NormalizeInviteCode(code)
	if !ok || token == "" {
		return nil
	}
	grace := time.Duration(0)
	if soft {
		grace = ReservationGrace
	}
	return s.repo.ReleaseReservation(ctx, code, hashToken(token), grace)
}

// ---------- 生成、查看、作废（需要登录） ----------

// InviteOptions 生成邀请码的参数，零值表示用默认值（网页生成时就是全用默认值）。
// 没有"次数"：一个邀请码只能提交一次注册申请
type InviteOptions struct {
	Note string
	TTL  time.Duration
}

// InviteDetail 邀请码及其溯源信息
type InviteDetail struct {
	InviteCode
	UsedBy        *User                // 用它提交注册申请的账号，还没用过为 nil。只含 accountColumns 里的列，没有 password_hash
	Verifications []InviteVerification // 验证记录，最新的在前
}

// Create 以 creatorID 的名义生成邀请码。命令行生成时 creatorID 传管理员 ID，和网页生成走同一套规则
func (s *InviteService) Create(ctx context.Context, creatorID int64, opts InviteOptions) (*InviteCode, error) {
	if !s.CanInvite(creatorID) {
		slog.WarnContext(ctx, "create invite rejected", "creator_id", creatorID, "reason", "forbidden")
		return nil, ErrInviteForbidden
	}
	note := strings.TrimSpace(opts.Note)
	if utf8.RuneCountInString(note) > maxInviteNoteLen {
		return nil, ErrInvalidInviteNote
	}
	// cmp.Or 返回第一个非零值：没指定（零值）就用默认值
	ttl := cmp.Or(opts.TTL, DefaultInviteTTL)
	if ttl < MinInviteTTL || ttl > MaxInviteTTL {
		return nil, ErrInvalidInviteOptions
	}

	c := &InviteCode{CreatedBy: creatorID, Note: note}
	// 36^6 ≈ 21.8 亿种组合，撞上已有的码概率极小；撞上了就换一个，和随机用户 ID 的处理一样
	for range maxCodeAttempts {
		code, err := s.newCode()
		if err != nil {
			return nil, fmt.Errorf("generate invite code: %w", err)
		}
		c.Code = code
		err = s.repo.CreateInvite(ctx, c, ttl)
		if errors.Is(err, ErrDuplicateInviteCode) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// 不记录邀请码明文：日志的读者比数据库多，拿到码就能去验证
		slog.InfoContext(ctx, "invite created", "invite_id", c.ID, "creator_id", creatorID, "expires_at", c.ExpiresAt.UTC())
		return c, nil
	}
	return nil, fmt.Errorf("generate invite code: still conflicting after %d attempts", maxCodeAttempts)
}

// List 列出 creatorID 生成的邀请码（最新的在前）及其溯源信息
func (s *InviteService) List(ctx context.Context, creatorID int64) ([]InviteDetail, error) {
	codes, err := s.repo.ListInvitesByCreator(ctx, creatorID, inviteListLimit)
	if err != nil {
		return nil, err
	}
	return s.withDetails(ctx, codes)
}

// Get 查看 creatorID 生成的某个邀请码及其溯源信息
func (s *InviteService) Get(ctx context.Context, creatorID, id int64) (*InviteDetail, error) {
	c, err := s.repo.FindInvite(ctx, id, creatorID)
	if err != nil {
		return nil, err
	}
	details, err := s.withDetails(ctx, []InviteCode{*c})
	if err != nil {
		return nil, err
	}
	return &details[0], nil
}

// Revoke 作废 creatorID 生成的邀请码。冻结中的也可以作废（正在填表的人提交时会失败）；
// 已作废、已使用的原样返回（幂等），已经注册的账号不受影响
func (s *InviteService) Revoke(ctx context.Context, creatorID, id int64) (*InviteDetail, error) {
	c, err := s.repo.RevokeInvite(ctx, id, creatorID)
	if err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "invite revoked", "invite_id", c.ID, "creator_id", creatorID)
	details, err := s.withDetails(ctx, []InviteCode{*c})
	if err != nil {
		return nil, err
	}
	return &details[0], nil
}

// withDetails 用两次批量查询补齐每个码的使用者和验证记录，保持 codes 原有顺序
func (s *InviteService) withDetails(ctx context.Context, codes []InviteCode) ([]InviteDetail, error) {
	ids := make([]int64, 0, len(codes))
	for _, c := range codes {
		ids = append(ids, c.ID)
	}
	users, err := s.repo.ListUsersByInvites(ctx, ids)
	if err != nil {
		return nil, err
	}
	verifications, err := s.repo.ListVerifications(ctx, ids)
	if err != nil {
		return nil, err
	}

	usedBy := make(map[int64]*User, len(users))
	for i := range users {
		if id := users[i].InviteCodeID; id != nil {
			usedBy[*id] = &users[i]
		}
	}
	byInvite := map[int64][]InviteVerification{}
	for _, v := range verifications {
		if v.InviteCodeID != nil {
			byInvite[*v.InviteCodeID] = append(byInvite[*v.InviteCodeID], v)
		}
	}
	details := make([]InviteDetail, 0, len(codes))
	for _, c := range codes {
		details = append(details, InviteDetail{InviteCode: c, UsedBy: usedBy[c.ID], Verifications: byInvite[c.ID]})
	}
	return details, nil
}
