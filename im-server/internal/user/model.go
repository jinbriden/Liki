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
	// ErrTooManyAttempts 同一个账号的登录尝试太频繁（NFR-05：防止针对单个账号暴力破解密码）
	ErrTooManyAttempts = errors.New("too many login attempts")
	ErrUserNotFound    = errors.New("user not found")
	// ErrDuplicateID 插入时用户 ID 与已有账号冲突，service 会换一个 ID 重试
	ErrDuplicateID = errors.New("duplicate user id")

	// ErrAccountPending / ErrAccountRejected 密码正确，但账号还没通过审核（FR-22）。
	// 只在密码校验通过之后返回，不知道密码的人无法借此得知某个 ID 的审核状态。
	ErrAccountPending  = errors.New("account pending review")
	ErrAccountRejected = errors.New("account rejected")
	// ErrInvalidStatusTransition 审核操作不适用于账号的当前状态（如拒绝一个已通过的账号）
	ErrInvalidStatusTransition = errors.New("invalid account status transition")

	// ErrInvalidInvite 邀请码不存在、格式不对、已过期、已使用或已作废（FR-23）。
	// 几种情况统一返回它，不告诉对方具体原因。
	ErrInvalidInvite = errors.New("invalid invite code")
	// ErrInviteInUse 邀请码有效，但正被别人冻结（对方验证通过后正在填写注册表单）
	ErrInviteInUse = errors.New("invite code in use")
	// ErrInviteVerifyTooFrequent 这个码 1 分钟内已经验证成功过一次
	ErrInviteVerifyTooFrequent = errors.New("invite code verified too frequently")
	// ErrInviteVerifyDailyLimit 这个码最近 24 小时内验证成功的次数已达上限
	ErrInviteVerifyDailyLimit = errors.New("invite code daily verification limit reached")
	// ErrVerifyIPLimit 这个 IP 最近 24 小时内的验证次数已达上限（防止暴力猜码）
	ErrVerifyIPLimit = errors.New("ip daily verification limit reached")
	// ErrReservationLost 冻结已失效（到期、被作废、令牌不对），续期和提交注册时返回，需要重新验证
	ErrReservationLost = errors.New("invite reservation lost")
	// ErrInviteNotFound 邀请码不存在，或不是当前用户生成的（查看、作废时）
	ErrInviteNotFound = errors.New("invite code not found")
	// ErrDuplicateInviteCode 新生成的邀请码和已有的重复，service 会重新生成
	ErrDuplicateInviteCode = errors.New("duplicate invite code")
	// ErrInviteForbidden 当前账号没有生成邀请码的权限（现阶段只有管理员有）
	ErrInviteForbidden      = errors.New("not allowed to create invite codes")
	ErrInvalidInviteNote    = errors.New("invalid invite note")
	ErrInvalidInviteOptions = errors.New("invalid invite options")
)

// AccountStatus 账号审核状态，取值和数据库 CHECK 约束 users_status_check 一致
type AccountStatus string

const (
	StatusPending  AccountStatus = "pending"  // 待审核：注册后的初始状态，不能登录
	StatusActive   AccountStatus = "active"   // 已通过：能登录、能被搜索到
	StatusRejected AccountStatus = "rejected" // 已拒绝：不能登录，管理员可以再批准
)

// User 对应 users 表。
// ID 是随机 10 位数，唯一标识账号（登录也使用 ID）；Username 不唯一，同时作为昵称展示。
type User struct {
	ID           int64 `gorm:"primaryKey;autoIncrement:false"`
	Username     string
	PasswordHash string
	AvatarURL    string `gorm:"column:avatar_url"`
	// Status 零值时 GORM 跳过该列，由数据库默认值 'pending' 兜底；注册时 service 会明确写入
	Status AccountStatus `gorm:"default:pending"`
	// InviteCodeID 注册时使用的邀请码；引入邀请码之前的账号为 nil
	InviteCodeID *int64
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

// InviteCode 对应 invite_codes 表（FR-23）。一个码只能提交一次注册申请；记录永不删除，作废只写 RevokedAt。
type InviteCode struct {
	ID        int64
	Code      string // 6 位数字 + 大写字母，明文存储
	CreatedBy int64  // 生成它的账号；命令行生成的也记在管理员账号名下
	Note      string
	// ExpiresAt 由数据库按 now() + 有效期计算，不用 Go 的 time.Now()；冻结、解冻都不改它
	ExpiresAt time.Time
	UsedAt    *time.Time // 提交注册申请的时间，非 nil 即已使用
	RevokedAt *time.Time
	// 冻结（验证通过、正在填写注册表单）：一份到 ReservedUntil 为止的租约，三个字段同时为 nil 或同时有值
	ReservedAt      *time.Time
	ReservedUntil   *time.Time
	ReservationHash *string   // 冻结令牌的 SHA-256，不对外返回
	CreatedAt       time.Time `gorm:"autoCreateTime:false;default:now()"`
}

// InviteState 邀请码当前的状态。不存库，按字段推算
type InviteState string

const (
	InviteActive   InviteState = "active"   // 可以验证
	InviteReserved InviteState = "reserved" // 冻结中：有人验证通过，正在填写注册表单
	InviteUsed     InviteState = "used"     // 已提交注册申请，永久失效
	InviteExpired  InviteState = "expired"  // 已过期
	InviteRevoked  InviteState = "revoked"  // 已作废
)

// StateAt 推算邀请码在 now 时刻的状态。几种情况同时成立时按 作废 > 已使用 > 过期 > 冻结 的优先级显示：
// 作废是管理员的主动操作，最值得看到；已使用说明码已经发挥了作用，比"过期"信息量大。
// 这里的 now 只用于展示；能不能验证、能不能提交，以数据库里用 now() 做的判断为准。
func (c *InviteCode) StateAt(now time.Time) InviteState {
	switch {
	case c.RevokedAt != nil:
		return InviteRevoked
	case c.UsedAt != nil:
		return InviteUsed
	case !now.Before(c.ExpiresAt):
		return InviteExpired
	case c.ReservedUntil != nil && now.Before(*c.ReservedUntil):
		return InviteReserved
	default:
		return InviteActive
	}
}

// VerifyResult 一次验证的结果，和 invite_verifications.result 的取值一致
type VerifyResult string

const (
	VerifyReserved VerifyResult = "reserved" // 验证成功并冻结
	VerifyInvalid  VerifyResult = "invalid"  // 码不存在、过期、已使用或已作废
	VerifyInUse    VerifyResult = "in_use"   // 码正被别人冻结
	VerifyLimited  VerifyResult = "limited"  // 超出这个码的验证频率
)

// InviteVerification 对应 invite_verifications 表：一次验证尝试（限流计数 + 溯源）
type InviteVerification struct {
	ID           int64
	IP           string
	InviteCodeID *int64 // 输入的码不存在时为 nil
	Result       VerifyResult
	CreatedAt    time.Time `gorm:"autoCreateTime:false;default:now()"`
}
