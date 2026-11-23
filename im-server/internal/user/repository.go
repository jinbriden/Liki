package user

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Repository interface {
	// CreateWithInvite 在同一个事务里使用邀请码并插入用户（FR-23），u.ID 由调用方预先生成。
	// 邀请码必须正被 tokenHash 对应的令牌冻结且仍然有效，否则 → ErrReservationLost；ID 冲突 → ErrDuplicateID。
	// 任何一步失败都整体回滚，邀请码不会被白白用掉。成功时回填 u.InviteCodeID、u.CreatedAt。
	CreateWithInvite(ctx context.Context, u *User, code, tokenHash string) error
	FindByID(ctx context.Context, id int64) (*User, error)
	// FindByIDs 批量查询，不存在的 ID 直接跳过。只取资料字段，不读 password_hash。
	FindByIDs(ctx context.Context, ids []int64) ([]User, error)
	// SearchByUsernameOrID 查 lower(username) = lower(username) 或 id = id 的已通过账号，最多 limit 条。
	// id 传 0 表示不按 ID 匹配（0 不在合法 ID 范围内，CHECK 约束保证不会命中）。
	SearchByUsernameOrID(ctx context.Context, username string, id int64, limit int) ([]User, error)

	// ListByStatus 按注册时间先后列出某个状态的账号，最多 limit 条。不读 password_hash。
	ListByStatus(ctx context.Context, status AccountStatus, limit int) ([]User, error)
	// UpdateStatus 账号当前状态属于 from 时才改成 to，返回是否真的改了（条件更新，状态转换规则由数据库原子地保证）
	UpdateStatus(ctx context.Context, id int64, to AccountStatus, from ...AccountStatus) (bool, error)

	// ReserveInvite 验证邀请码并冻结（FR-23），连同限流检查在一个事务里完成，每次尝试都写一条验证记录。
	// 失败时返回 ErrVerifyIPLimit / ErrInvalidInvite / ErrInviteInUse / ErrInviteVerifyTooFrequent / ErrInviteVerifyDailyLimit，
	// 此时验证记录已经提交（失败的尝试也要计入 IP 的次数）。
	ReserveInvite(ctx context.Context, req ReserveRequest) (*InviteCode, error)
	// CheckReservation 只读地检查邀请码是否正被 tokenHash 冻结且仍然有效。否 → ErrReservationLost
	CheckReservation(ctx context.Context, code, tokenHash string) error
	// RenewReservation 续期：冻结到 min(now + lease, 冻结时间 + maxHold)。冻结已失效或超过 maxHold → ErrReservationLost
	RenewReservation(ctx context.Context, code, tokenHash string, lease, maxHold time.Duration) (*InviteCode, error)
	// ReleaseReservation 解冻。grace 为 0 时立即解冻；大于 0 时只保留 grace 这么久（软释放，期间可以续期恢复）。
	// 令牌不对、已经解冻、已使用时什么也不做（幂等）
	ReleaseReservation(ctx context.Context, code, tokenHash string, grace time.Duration) error

	// CreateInvite 插入邀请码，过期时间 = 数据库 now() + ttl。码重复 → ErrDuplicateInviteCode。
	// 成功时回填 ID、ExpiresAt、CreatedAt 等字段。
	CreateInvite(ctx context.Context, c *InviteCode, ttl time.Duration) error
	// FindInvite 查 creatorID 生成的某个邀请码。不存在或不是他生成的 → ErrInviteNotFound
	FindInvite(ctx context.Context, id, creatorID int64) (*InviteCode, error)
	// FindInvitesByIDs 批量查询，不存在的 ID 直接跳过
	FindInvitesByIDs(ctx context.Context, ids []int64) ([]InviteCode, error)
	// ListInvitesByCreator 列出 creatorID 生成的邀请码，最新的在前，最多 limit 条
	ListInvitesByCreator(ctx context.Context, creatorID int64, limit int) ([]InviteCode, error)
	// RevokeInvite 作废 creatorID 生成的、还没使用的邀请码（冻结中的也可以，冻结随之失效），返回作废后的记录。
	// 已作废或已使用的不再改动，原样返回（幂等）；不存在或不是他生成的 → ErrInviteNotFound
	RevokeInvite(ctx context.Context, id, creatorID int64) (*InviteCode, error)
	// ListUsersByInvites 列出用这些邀请码注册的账号，按注册时间先后。不读 password_hash。
	ListUsersByInvites(ctx context.Context, inviteIDs []int64) ([]User, error)
	// ListVerifications 列出这些邀请码的验证记录，最新的在前
	ListVerifications(ctx context.Context, inviteIDs []int64) ([]InviteVerification, error)
}

// ReserveRequest 验证并冻结邀请码的参数。限流的数值由 service 决定，repository 只负责在事务里原子地检查
type ReserveRequest struct {
	Code      string // 已规范化（大写）的邀请码
	IP        string
	TokenHash string        // 冻结令牌的 SHA-256
	Lease     time.Duration // 冻结多久（之后靠续期）

	IPPerDay        int           // 每个 IP 24 小时内最多验证几次（不论码是否有效）
	CodePerDay      int           // 每个码 24 小时内最多验证成功几次
	CodeMinInterval time.Duration // 每个码两次验证成功之间的最短间隔
}

// profileColumns 查询用户资料时只取这几列，不把 password_hash 读进内存
var profileColumns = []string{"id", "username", "avatar_url"}

// accountColumns 审核、邀请码溯源时展示账号用的列，同样不读 password_hash
var accountColumns = []string{"id", "username", "avatar_url", "status", "invite_code_id", "created_at"}

type gormRepository struct {
	db *gorm.DB // *gorm.DB 是 gorm 的数据库连接对象，用于执行数据库操作
}

func NewRepository(db *gorm.DB) Repository {
	return &gormRepository{db: db}
}

// translate 补上手写 SQL（Raw(...).Scan）的错误翻译。
// gorm.Config.TranslateError 只在 db.AddError 里生效，而 Raw 走的 Row 回调把驱动返回的错误直接赋给了 db.Error，
// 拿到的是原始的 *pgconn.PgError，errors.Is(err, gorm.ErrDuplicatedKey) 判断不出来。
// 这里用同一个翻译器（postgres 驱动的 Dialector）手动翻译一次，之后的判断和 Create / Update 一样
func (r *gormRepository) translate(err error) error {
	if t, ok := r.db.Dialector.(gorm.ErrorTranslator); ok && err != nil {
		return t.Translate(err)
	}
	return err
}

// 邀请码条件。几处 SQL 共用同一段条件，判断不会不一致；时间一律用数据库的 now()，不依赖应用服务器的时钟。
const (
	// inviteUsable 码本身有效：没用过、没作废、没过期
	inviteUsable = "used_at IS NULL AND revoked_at IS NULL AND expires_at > now()"
	// heldByToken 码有效，并且正被这个令牌冻结（参数：code, tokenHash）
	heldByToken = "code = ? AND reservation_hash = ? AND reserved_until > now() AND " + inviteUsable
)

func (r *gormRepository) CreateWithInvite(ctx context.Context, u *User, code, tokenHash string) error {
	// Transaction：回调返回 error 就回滚，返回 nil 就提交，作用类似 Spring 的 @Transactional。
	// 回调里所有操作都必须用 tx 而不是 r.db，否则不在同一个事务里。
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 条件 UPDATE 使用邀请码：WHERE 里同时检查"正被我冻结、仍然有效"，并发提交时数据库的行锁让它们排队，
		// 只有一个能更新成功（used_at 写入后，后面的都影响 0 行）。冻结字段一并清空
		var inviteID int64
		res := tx.Raw(`
UPDATE invite_codes
SET used_at = now(), reserved_at = NULL, reserved_until = NULL, reservation_hash = NULL
WHERE `+heldByToken+`
RETURNING id`, code, tokenHash).Scan(&inviteID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrReservationLost
		}

		u.InviteCodeID = &inviteID
		// 唯一约束有两个：主键，和 uq_users_invite_code（一个码最多对应一个账号）。
		// 后者不会被触发：上面的 UPDATE 持有这个码的行锁并写入了 used_at，别的事务用不了同一个码。
		// 所以重复键错误一定是 ID 冲突。返回 error 会让整个事务回滚，邀请码恢复成冻结状态，service 换一个 ID 重试
		err := tx.Create(u).Error
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return ErrDuplicateID
		}
		return err
	})
}

// FindByID 按主键查询，查不到时返回 ErrUserNotFound。
func (r *gormRepository) FindByID(ctx context.Context, id int64) (*User, error) {
	var u User
	// First 生成参数化查询：SELECT ... WHERE id = $1 ORDER BY id LIMIT 1
	err := r.db.WithContext(ctx).First(&u, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (r *gormRepository) FindByIDs(ctx context.Context, ids []int64) ([]User, error) {
	if len(ids) == 0 {
		return nil, nil // 避免生成 WHERE id IN () 这样的非法 SQL
	}
	var users []User
	// GORM 把切片参数展开成 IN ($1, $2, ...)，仍然是参数化查询
	err := r.db.WithContext(ctx).Select(profileColumns).Where("id IN ?", ids).Find(&users).Error
	return users, err
}

func (r *gormRepository) SearchByUsernameOrID(ctx context.Context, username string, id int64, limit int) ([]User, error) {
	var users []User
	// 两个条件分别命中 idx_users_username_lower 和主键索引，PostgreSQL 可以用 BitmapOr 合并。
	// ID 精确命中的排在最前面，其余按 ID 排序，保证结果稳定。
	// OR 两边必须加括号：AND 的优先级比 OR 高，不加括号就成了 "用户名匹配 OR (ID 匹配 AND 已通过)"，
	// 按用户名能搜到待审核的账号
	err := r.db.WithContext(ctx).
		Select(profileColumns).
		Where("(lower(username) = lower(?) OR id = ?) AND status = ?", username, id, StatusActive).
		Order(clause.OrderBy{Expression: clause.Expr{SQL: "id = ? DESC, id", Vars: []any{id}}}).
		Limit(limit).
		Find(&users).Error
	return users, err
}

func (r *gormRepository) ListByStatus(ctx context.Context, status AccountStatus, limit int) ([]User, error) {
	var users []User
	err := r.db.WithContext(ctx).
		Select(accountColumns).
		Where("status = ?", status).
		Order("created_at, id").
		Limit(limit).
		Find(&users).Error
	return users, err
}

func (r *gormRepository) UpdateStatus(ctx context.Context, id int64, to AccountStatus, from ...AccountStatus) (bool, error) {
	// 只有一条 UPDATE，本身就是原子的，不需要事务。思路和 read_seq 只增不减一样：
	// 把"当前状态允许这样转换"写进 WHERE，并发的两次操作（如同时批准和拒绝）只有一个能生效
	res := r.db.WithContext(ctx).Model(&User{}).
		Where("id = ? AND status IN ?", id, from).
		Update("status", to)
	return res.RowsAffected == 1, res.Error
}

func (r *gormRepository) ReserveInvite(ctx context.Context, req ReserveRequest) (*InviteCode, error) {
	// outcome 是这次验证的业务结果。失败的验证也要提交（验证记录要计入 IP 的次数），
	// 所以事务回调对业务失败返回 nil（提交），把结果放在 outcome 里带出来；只有数据库出错才返回 error（回滚）
	var outcome error
	var inv InviteCode
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1. 同一个 IP 的验证串行执行（事务级咨询锁，提交时自动释放），"先数再写"之间不会被同一 IP 的并发请求插队
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", "invite-verify:"+req.IP).Error; err != nil {
			return err
		}
		var ipCount int64
		if err := tx.Raw(
			"SELECT count(*) FROM invite_verifications WHERE ip = ? AND created_at > now() - interval '24 hours'", req.IP,
		).Scan(&ipCount).Error; err != nil {
			return err
		}
		if ipCount >= int64(req.IPPerDay) {
			outcome = ErrVerifyIPLimit // 超出 IP 限制的请求不再记录，否则攻击时这张表会无限增长
			return nil
		}

		record := func(inviteID *int64, result VerifyResult) error {
			return tx.Exec("INSERT INTO invite_verifications (ip, invite_code_id, result) VALUES (?, ?, ?)",
				req.IP, inviteID, result).Error
		}

		// 2. 锁住这个码的行（FOR UPDATE）：两个人同时验证同一个码，后到的要等前一个事务提交，
		//    看到的是已经被冻结的状态，不会同时冻结成功
		var row struct {
			ID     int64
			Usable bool // 没用过、没作废、没过期
			Held   bool // 正被冻结（冻结未到期）
		}
		res := tx.Raw(`
SELECT id, (`+inviteUsable+`) AS usable, COALESCE(reserved_until > now(), false) AS held
FROM invite_codes WHERE code = ? FOR UPDATE`, req.Code).Scan(&row)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			outcome = ErrInvalidInvite
			return record(nil, VerifyInvalid)
		}
		if !row.Usable {
			outcome = ErrInvalidInvite
			return record(&row.ID, VerifyInvalid)
		}
		if row.Held {
			outcome = ErrInviteInUse
			return record(&row.ID, VerifyInUse)
		}

		// 3. 这个码的验证频率：只数验证成功（冻结）的次数
		var counts struct {
			Day    int64
			Recent int64
		}
		if err := tx.Raw(`
SELECT count(*) FILTER (WHERE created_at > now() - interval '24 hours') AS day,
       count(*) FILTER (WHERE created_at > now() - make_interval(secs => ?)) AS recent
FROM invite_verifications WHERE invite_code_id = ? AND result = ?`,
			req.CodeMinInterval.Seconds(), row.ID, VerifyReserved).Scan(&counts).Error; err != nil {
			return err
		}
		switch {
		case counts.Recent > 0:
			outcome = ErrInviteVerifyTooFrequent
			return record(&row.ID, VerifyLimited)
		case counts.Day >= int64(req.CodePerDay):
			outcome = ErrInviteVerifyDailyLimit
			return record(&row.ID, VerifyLimited)
		}

		// 4. 冻结。expires_at 不动（有效期不因冻结而刷新）
		if err := tx.Raw(`
UPDATE invite_codes
SET reserved_at = now(), reserved_until = now() + make_interval(secs => ?), reservation_hash = ?
WHERE id = ?
RETURNING *`, req.Lease.Seconds(), req.TokenHash, row.ID).Scan(&inv).Error; err != nil {
			return err
		}
		return record(&row.ID, VerifyReserved)
	})
	if err != nil {
		return nil, err
	}
	if outcome != nil {
		return nil, outcome
	}
	return &inv, nil
}

func (r *gormRepository) CheckReservation(ctx context.Context, code, tokenHash string) error {
	var n int64
	if err := r.db.WithContext(ctx).Model(&InviteCode{}).Where(heldByToken, code, tokenHash).Count(&n).Error; err != nil {
		return err
	}
	if n == 0 {
		return ErrReservationLost
	}
	return nil
}

func (r *gormRepository) RenewReservation(ctx context.Context, code, tokenHash string, lease, maxHold time.Duration) (*InviteCode, error) {
	var inv InviteCode
	// LEAST：冻结总时长不超过 maxHold（从冻结时算起），防止一个页面一直开着把码永久占住
	res := r.db.WithContext(ctx).Raw(`
UPDATE invite_codes
SET reserved_until = LEAST(now() + make_interval(secs => ?), reserved_at + make_interval(secs => ?))
WHERE `+heldByToken+` AND reserved_at + make_interval(secs => ?) > now()
RETURNING *`, lease.Seconds(), maxHold.Seconds(), code, tokenHash, maxHold.Seconds()).Scan(&inv)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, ErrReservationLost
	}
	return &inv, nil
}

func (r *gormRepository) ReleaseReservation(ctx context.Context, code, tokenHash string, grace time.Duration) error {
	db := r.db.WithContext(ctx)
	if grace <= 0 {
		return db.Exec(`
UPDATE invite_codes SET reserved_at = NULL, reserved_until = NULL, reservation_hash = NULL
WHERE code = ? AND reservation_hash = ? AND used_at IS NULL`, code, tokenHash).Error
	}
	// 软释放：只把到期时间提前到 grace 之后（不延长），期间持有令牌的页面续期就能恢复（刷新页面的情况）
	return db.Exec(`
UPDATE invite_codes SET reserved_until = LEAST(reserved_until, now() + make_interval(secs => ?))
WHERE code = ? AND reservation_hash = ? AND reserved_until > now() AND used_at IS NULL`,
		grace.Seconds(), code, tokenHash).Error
}

func (r *gormRepository) CreateInvite(ctx context.Context, c *InviteCode, ttl time.Duration) error {
	// 过期时间由数据库按 now() 计算（和 created_at 同一个时钟），所以手写 INSERT 而不是 db.Create(c)：
	// GORM 的 Create 只能写入 Go 里算好的值。RETURNING * 把整行（含 id、expires_at、created_at）写回 c
	err := r.db.WithContext(ctx).Raw(`
INSERT INTO invite_codes (code, created_by, note, expires_at)
VALUES (?, ?, ?, now() + make_interval(secs => ?))
RETURNING *`, c.Code, c.CreatedBy, c.Note, ttl.Seconds()).Scan(c).Error
	err = r.translate(err)
	switch {
	case errors.Is(err, gorm.ErrDuplicatedKey):
		return ErrDuplicateInviteCode // invite_codes 上的唯一约束只有 code（主键由序列生成）
	case errors.Is(err, gorm.ErrForeignKeyViolated):
		return ErrUserNotFound // created_by 对应的账号不存在
	}
	return err
}

func (r *gormRepository) FindInvite(ctx context.Context, id, creatorID int64) (*InviteCode, error) {
	var c InviteCode
	// 不是自己生成的码和不存在一样处理，不让别人探测码的 ID
	err := r.db.WithContext(ctx).Where("id = ? AND created_by = ?", id, creatorID).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrInviteNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *gormRepository) FindInvitesByIDs(ctx context.Context, ids []int64) ([]InviteCode, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var codes []InviteCode
	err := r.db.WithContext(ctx).Where("id IN ?", ids).Find(&codes).Error
	return codes, err
}

func (r *gormRepository) ListInvitesByCreator(ctx context.Context, creatorID int64, limit int) ([]InviteCode, error) {
	var codes []InviteCode
	// 命中索引 idx_invite_codes_creator (created_by, created_at DESC)
	err := r.db.WithContext(ctx).
		Where("created_by = ?", creatorID).
		Order("created_at DESC, id DESC").
		Limit(limit).
		Find(&codes).Error
	return codes, err
}

func (r *gormRepository) RevokeInvite(ctx context.Context, id, creatorID int64) (*InviteCode, error) {
	var c InviteCode
	res := r.db.WithContext(ctx).Raw(`
UPDATE invite_codes
SET revoked_at = now(), reserved_at = NULL, reserved_until = NULL, reservation_hash = NULL
WHERE id = ? AND created_by = ? AND revoked_at IS NULL AND used_at IS NULL
RETURNING *`, id, creatorID).Scan(&c)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 1 {
		return &c, nil
	}
	// 没有改动：要么码不存在 / 不是他的（FindInvite 返回 ErrInviteNotFound），
	// 要么已经作废或已使用，作废没有意义，返回当前状态即可
	return r.FindInvite(ctx, id, creatorID)
}

func (r *gormRepository) ListUsersByInvites(ctx context.Context, inviteIDs []int64) ([]User, error) {
	if len(inviteIDs) == 0 {
		return nil, nil
	}
	var users []User
	// 命中唯一索引 uq_users_invite_code
	err := r.db.WithContext(ctx).
		Select(accountColumns).
		Where("invite_code_id IN ?", inviteIDs).
		Order("created_at, id").
		Find(&users).Error
	return users, err
}

func (r *gormRepository) ListVerifications(ctx context.Context, inviteIDs []int64) ([]InviteVerification, error) {
	if len(inviteIDs) == 0 {
		return nil, nil
	}
	var list []InviteVerification
	// host(ip)：INET 转成不带掩码的文本（如 203.0.113.7），Go 里按字符串接收
	err := r.db.WithContext(ctx).Raw(`
SELECT id, host(ip) AS ip, invite_code_id, result, created_at
FROM invite_verifications WHERE invite_code_id IN ?
ORDER BY created_at DESC, id DESC`, inviteIDs).Scan(&list).Error
	return list, err
}
