package user

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// 用户名、密码规则。只允许 ASCII 字符，所以 len（字节数）等于字符数。
// 密码最长 24 位，远低于 bcrypt 的 72 字节上限。
const (
	minUsernameLen = 3
	maxUsernameLen = 32
	minPasswordLen = 8
	maxPasswordLen = 24
)

// 用户 ID 是 [minUserID, maxUserID] 范围内的随机 10 位数
const (
	minUserID     = 1_000_000_000
	maxUserID     = 9_999_999_999
	maxIDAttempts = 5 // 生成的 ID 与已有账号冲突时的最大尝试次数
)

// IDGenerator 生成新用户 ID。抽成函数类型，测试时可以注入固定的 ID 序列。
type IDGenerator func() (int64, error)

// RandomUserID 用 crypto/rand 生成随机 10 位 ID。
// 使用加密安全的随机源，ID 不可预测，无法按顺序猜出其他账号或推算注册量。
func RandomUserID() (int64, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(maxUserID-minUserID+1))
	if err != nil {
		return 0, err
	}
	return minUserID + n.Int64(), nil
}

// TokenIssuer 签发访问令牌。service 只依赖这个接口，不依赖具体的 JWT 实现。
type TokenIssuer interface {
	Issue(userID int64) (token string, expiresAt time.Time, err error)
}

// Limiter 限流器，由 ratelimit 包的实现满足。按 Go 的习惯由使用方定义接口，user 模块不依赖具体实现
type Limiter interface {
	Allow(ctx context.Context, key string) bool
}

type Service struct {
	repo   Repository
	tokens TokenIssuer
	newID  IDGenerator
	// loginLimit 按目标账号限制登录尝试（NFR-05）。nil 表示不限制（单元测试默认如此）。
	// 按 IP 的限制在 HTTP 中间件里做；按账号的只能放在这里：要解析请求体才知道登录的是哪个账号
	loginLimit Limiter
}

// Option 可选配置。用"函数式选项"而不是加构造参数：不需要它的调用方（测试）不用改，
// 作用类似 Java 里 builder 的可选 setter
type Option func(*Service)

// WithLoginLimiter 按目标用户 ID 限制登录尝试次数
func WithLoginLimiter(l Limiter) Option {
	return func(s *Service) { s.loginLimit = l }
}

func NewService(repo Repository, tokens TokenIssuer, newID IDGenerator, opts ...Option) *Service {
	s := &Service{repo: repo, tokens: tokens, newID: newID}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Register 提交注册申请（FR-01、FR-22、FR-23）。用户名允许重复，按用户输入原样保存；账号由随机生成的 ID 唯一标识。
// 必须持有邀请码的冻结令牌（先通过 InviteService.Verify 验证），提交成功后邀请码立即失效（与审核结果无关）；
// 新账号是待审核状态，管理员批准后才能登录。
func (s *Service) Register(ctx context.Context, username, password, inviteCode, reservationToken string) (*User, error) {
	if err := validateUsername(username); err != nil {
		return nil, err
	}
	if err := validatePassword(password); err != nil {
		return nil, err
	}
	code, ok := NormalizeInviteCode(inviteCode)
	if !ok || reservationToken == "" {
		slog.InfoContext(ctx, "register rejected", "reason", "no_reservation") // 不记录邀请码和令牌
		return nil, ErrReservationLost
	}
	tokenHash := hashToken(reservationToken)
	// 先只读地检查一次冻结，必须放在 bcrypt 之前：bcrypt 故意设计得很慢（约 100ms CPU），
	// 如果先算哈希，别人拿无效的令牌反复提交，每次都要白白消耗服务器的算力。
	// 这一步只是快速拒绝，真正的判断在下面事务里的条件更新（两步之间冻结可能刚好到期、被作废）
	if err := s.repo.CheckReservation(ctx, code, tokenHash); err != nil {
		if errors.Is(err, ErrReservationLost) {
			slog.InfoContext(ctx, "register rejected", "reason", "reservation_lost")
		}
		return nil, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	u := &User{Username: username, PasswordHash: string(hash), Status: StatusPending}
	// 随机 ID 有极小概率和已有账号相同，此时主键约束会拒绝插入，换一个 ID 重试
	for range maxIDAttempts {
		if u.ID, err = s.newID(); err != nil {
			return nil, fmt.Errorf("generate user id: %w", err)
		}
		err = s.repo.CreateWithInvite(ctx, u, code, tokenHash)
		if errors.Is(err, ErrDuplicateID) {
			continue
		}
		if errors.Is(err, ErrReservationLost) {
			slog.InfoContext(ctx, "register rejected", "reason", "reservation_lost_in_tx")
			return nil, err
		}
		if err != nil {
			return nil, err
		}
		slog.InfoContext(ctx, "user registered", "new_user_id", u.ID, "invite_id", *u.InviteCodeID)
		return u, nil
	}
	return nil, fmt.Errorf("allocate user id: still conflicting after %d attempts", maxIDAttempts)
}

// LoginResult 登录成功后返回的数据
type LoginResult struct {
	User      *User
	Token     string
	ExpiresAt time.Time
}

// dummyHash 用于"用户不存在"时也做一次 bcrypt 比较，使响应时间和"密码错误"一致，
// 防止通过响应快慢判断某个 ID 是否存在。cost 必须和真实密码哈希相同，耗时才一致。
// sync.OnceValue 保证只在第一次用到时计算一次（类似 Java 的懒加载单例）。
var dummyHash = sync.OnceValue(func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("liki-dummy-password"), bcrypt.DefaultCost)
	if err != nil {
		panic(fmt.Sprintf("generate dummy hash: %v", err))
	}
	return h
})

// Login 用户 ID + 密码登录，成功后签发令牌。
// 用户不存在、密码错误、参数明显不合法，都返回同一个 ErrInvalidCredentials。
func (s *Service) Login(ctx context.Context, userID int64, password string) (*LoginResult, error) {
	// 只做"不可能正确"的快速拒绝，不校验密码复杂度：以后调整注册规则时，老用户仍要能登录
	if userID < minUserID || userID > maxUserID || password == "" || len(password) > maxPasswordLen {
		slog.InfoContext(ctx, "login rejected", "login_user_id", userID, "reason", "malformed")
		return nil, ErrInvalidCredentials
	}

	// 每次尝试都计数（不管成功与否、账号存不存在），放在查库和 bcrypt 之前：被限流的请求不消耗服务端算力
	if s.loginLimit != nil && !s.loginLimit.Allow(ctx, strconv.FormatInt(userID, 10)) {
		slog.WarnContext(ctx, "login rejected", "login_user_id", userID, "reason", "too_many_attempts")
		return nil, ErrTooManyAttempts
	}

	u, err := s.repo.FindByID(ctx, userID)
	if errors.Is(err, ErrUserNotFound) {
		_ = bcrypt.CompareHashAndPassword(dummyHash(), []byte(password))
		// 日志只给开发者看，可以写明具体原因；返回给客户端的仍是同一个错误
		slog.InfoContext(ctx, "login rejected", "login_user_id", userID, "reason", "user_not_found")
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}

	err = bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password))
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		slog.InfoContext(ctx, "login rejected", "login_user_id", userID, "reason", "wrong_password")
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		// 库里的哈希格式损坏等异常情况，属于服务端问题，不能当成"密码错误"掩盖掉
		return nil, fmt.Errorf("compare password: %w", err)
	}

	// 审核状态（FR-22）必须在密码校验通过之后再判断：
	// 反过来的话，不知道密码的人只要随便输一个密码，就能从错误码看出某个 ID 是待审核还是已拒绝
	switch u.Status {
	case StatusActive:
	case StatusPending:
		slog.InfoContext(ctx, "login rejected", "login_user_id", userID, "reason", "account_pending")
		return nil, ErrAccountPending
	case StatusRejected:
		slog.InfoContext(ctx, "login rejected", "login_user_id", userID, "reason", "account_rejected")
		return nil, ErrAccountRejected
	default:
		// 数据库 CHECK 约束保证不会出现其他值；真出现了是服务端问题，不能放行
		return nil, fmt.Errorf("unknown account status %q", u.Status)
	}

	token, expiresAt, err := s.tokens.Issue(u.ID)
	if err != nil {
		return nil, fmt.Errorf("issue token: %w", err)
	}
	slog.InfoContext(ctx, "user logged in", "login_user_id", u.ID) // 不记录令牌
	return &LoginResult{User: u, Token: token, ExpiresAt: expiresAt}, nil
}

// GetByID 查询用户，不存在时返回 ErrUserNotFound。
func (s *Service) GetByID(ctx context.Context, userID int64) (*User, error) {
	return s.repo.FindByID(ctx, userID)
}

// validateUsername 用户名：3-32 位，只能是英文字母或数字。
func validateUsername(username string) error {
	if len(username) < minUsernameLen || len(username) > maxUsernameLen {
		return ErrInvalidUsername
	}
	// 按字节遍历：中文等非 ASCII 字符的每个字节都 >= 0x80，会落到不合法分支
	for i := 0; i < len(username); i++ {
		c := username[i]
		if !isUpper(c) && !isLower(c) && !isDigit(c) {
			return ErrInvalidUsername
		}
	}
	return nil
}

// validatePassword 密码：8-24 位，只能使用可打印 ASCII 字符（不含空格），
// 并且至少包含一个大写字母、一个小写字母和一个数字。
func validatePassword(password string) error {
	if len(password) < minPasswordLen || len(password) > maxPasswordLen {
		return ErrInvalidPassword
	}
	var hasUpper, hasLower, hasDigit bool
	for i := 0; i < len(password); i++ {
		c := password[i]
		switch {
		case isUpper(c):
			hasUpper = true
		case isLower(c):
			hasLower = true
		case isDigit(c):
			hasDigit = true
		case c >= '!' && c <= '~':
			// 其余可打印 ASCII 符号（0x21-0x7E），允许但不计入任何一类
		default:
			// 空格、控制字符、非 ASCII 字符（中文、emoji 等）
			return ErrInvalidPassword
		}
	}
	if !hasUpper || !hasLower || !hasDigit {
		return ErrInvalidPassword
	}
	return nil
}

func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
func isLower(c byte) bool { return c >= 'a' && c <= 'z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// GetProfiles 批量查询用户资料，不存在的 ID 直接跳过，返回顺序不保证。
func (s *Service) GetProfiles(ctx context.Context, ids []int64) ([]Profile, error) {
	users, err := s.repo.FindByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	return toProfiles(users), nil
}

// SearchProfiles 按用户名（不区分大小写，完全匹配，走 lower(username) 索引）搜索；
// keyword 是 10 位数字时再加上按 ID 精确匹配（纯数字用户名也合法，两种都要查，合并去重）。最多 limit 条。
// 同一个用户同时命中两个条件时，SQL 的 OR 只返回一行，天然去重。
func (s *Service) SearchProfiles(ctx context.Context, keyword string, limit int) ([]Profile, error) {
	keyword = strings.TrimSpace(keyword)
	// 超过用户名最大长度的输入既不可能是用户名，也不可能是 10 位 ID，不必查库
	if keyword == "" || len(keyword) > maxUsernameLen || limit <= 0 {
		return nil, nil
	}
	users, err := s.repo.SearchByUsernameOrID(ctx, keyword, parseUserID(keyword), limit)
	if err != nil {
		return nil, err
	}
	return toProfiles(users), nil
}

// ParseID 解析 10 位用户 ID（命令行工具、配置项用），不是合法 ID 时 ok 为 false
func ParseID(s string) (id int64, ok bool) {
	id = parseUserID(strings.TrimSpace(s))
	return id, id != 0
}

// parseUserID keyword 恰好是 10 位数字时返回对应 ID，否则返回 0。
// 不能只用 strconv.ParseInt：它接受 "+123456789" 这样带符号的写法。
func parseUserID(keyword string) int64 {
	if len(keyword) != 10 {
		return 0
	}
	for i := 0; i < len(keyword); i++ {
		if !isDigit(keyword[i]) {
			return 0
		}
	}
	id, err := strconv.ParseInt(keyword, 10, 64)
	if err != nil || id < minUserID {
		return 0 // 以 0 开头的 10 位数不在合法范围内
	}
	return id
}

func toProfiles(users []User) []Profile {
	profiles := make([]Profile, 0, len(users))
	for _, u := range users {
		profiles = append(profiles, Profile{ID: u.ID, Username: u.Username, AvatarURL: u.AvatarURL})
	}
	return profiles
}
