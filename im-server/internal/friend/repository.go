package friend

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Repository 好友模块的数据访问。数据库错误在这里翻译成业务哨兵错误，service 不 import gorm。
type Repository interface {
	// CreateRequest 插入一条待处理申请，成功后 r.ID、r.Status、r.CreatedAt 由数据库回填。
	// 两人之间已有待处理申请（命中 uq_friend_requests_pending_pair）→ ErrPendingExists；
	// to_user_id 对应的用户不存在（外键约束）→ ErrTargetUserNotFound。
	CreateRequest(ctx context.Context, r *FriendRequest) error

	// FindPendingBetween 查两人之间（不分方向）的待处理申请，没有时返回 ErrRequestNotFound。
	FindPendingBetween(ctx context.Context, a, b int64) (*FriendRequest, error)

	// FindRequestByID 按 ID 查申请，没有时返回 ErrRequestNotFound。
	FindRequestByID(ctx context.Context, id int64) (*FriendRequest, error)

	// ListIncomingPending 发给 toUserID 的待处理申请，按创建时间倒序。
	ListIncomingPending(ctx context.Context, toUserID int64) ([]FriendRequest, error)

	// ListOutgoing fromUserID 发出的申请（含已处理），按创建时间倒序，最多 limit 条。
	ListOutgoing(ctx context.Context, fromUserID int64, limit int) ([]FriendRequest, error)

	// ListPendingInvolving userID 与 others 中任意一人之间（不分方向）的待处理申请，搜索时用来算关系状态。
	ListPendingInvolving(ctx context.Context, userID int64, others []int64) ([]FriendRequest, error)

	// Accept 在一个事务里：把申请改为 accepted 并写 handled_at，再插入两条 friendships 记录。
	// 只更新 id = requestID AND to_user_id = toUserID AND status = 'pending' 的行；
	// 影响 0 行（并发下已被处理）→ ErrRequestNotPending。
	Accept(ctx context.Context, requestID, toUserID int64) error

	// Reject 把申请改为 rejected 并写 handled_at，更新条件和 0 行时的返回同 Accept。
	Reject(ctx context.Context, requestID, toUserID int64) error

	// AreFriends 两人是否是好友。
	AreFriends(ctx context.Context, a, b int64) (bool, error)

	// ListFriendships userID 的全部好友关系（FriendID 是好友，CreatedAt 是成为好友的时间）。
	ListFriendships(ctx context.Context, userID int64) ([]Friendship, error)

	// FilterFriends 返回 candidates 中是 userID 好友的那些 ID，搜索时用来算关系状态。
	FilterFriends(ctx context.Context, userID int64, candidates []int64) ([]int64, error)

	// DeleteFriendship 删除 (a, b) 和 (b, a) 两条记录。本来就不是好友时不报错。
	DeleteFriendship(ctx context.Context, a, b int64) error
}

type gormRepository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) Repository {
	return &gormRepository{db: db}
}

// CreateRequest 主键是 BIGSERIAL 不会冲突，所以 ErrDuplicatedKey 只可能来自待处理唯一索引 uq_friend_requests_pending_pair。
func (r *gormRepository) CreateRequest(ctx context.Context, req *FriendRequest) error {
	// Status 零值时 GORM 跳过该列，用数据库默认值 'pending'；ID、Status、CreatedAt 通过 RETURNING 回填
	err := r.db.WithContext(ctx).Create(req).Error
	switch {
	case errors.Is(err, gorm.ErrDuplicatedKey):
		return ErrPendingExists
	case errors.Is(err, gorm.ErrForeignKeyViolated):
		// from_user_id 来自令牌，一定存在，所以外键冲突只可能是 to_user_id 不存在
		return ErrTargetUserNotFound
	}
	return err
}

func (r *gormRepository) FindPendingBetween(ctx context.Context, a, b int64) (*FriendRequest, error) {
	var req FriendRequest
	// 条件写成和唯一索引相同的表达式，直接命中 uq_friend_requests_pending_pair。
	// Take 不加 ORDER BY（First 会按主键排序）；唯一索引保证最多一行。
	where := "LEAST(from_user_id, to_user_id) = ? AND GREATEST(from_user_id, to_user_id) = ? AND status = ?"
	err := r.db.WithContext(ctx).Where(where, min(a, b), max(a, b), StatusPending).Take(&req).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRequestNotFound
	}
	if err != nil {
		return nil, err
	}
	return &req, nil
}

func (r *gormRepository) FindRequestByID(ctx context.Context, id int64) (*FriendRequest, error) {
	var req FriendRequest
	err := r.db.WithContext(ctx).Take(&req, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRequestNotFound
	}
	if err != nil {
		return nil, err
	}
	return &req, nil
}

func (r *gormRepository) ListIncomingPending(ctx context.Context, toUserID int64) ([]FriendRequest, error) {
	var reqs []FriendRequest
	err := r.db.WithContext(ctx).
		Where("to_user_id = ? AND status = ?", toUserID, StatusPending).
		Order("created_at DESC, id DESC"). // 同一微秒内创建的按 ID 排，保证顺序稳定
		Find(&reqs).Error
	return reqs, err
}

func (r *gormRepository) ListOutgoing(ctx context.Context, fromUserID int64, limit int) ([]FriendRequest, error) {
	var reqs []FriendRequest
	err := r.db.WithContext(ctx).
		Where("from_user_id = ?", fromUserID).
		Order("created_at DESC, id DESC").
		Limit(limit).
		Find(&reqs).Error
	return reqs, err
}

func (r *gormRepository) ListPendingInvolving(ctx context.Context, userID int64, others []int64) ([]FriendRequest, error) {
	if len(others) == 0 {
		return nil, nil // 避免生成 IN () 这样的非法 SQL
	}
	var reqs []FriendRequest
	err := r.db.WithContext(ctx).
		Where("status = ? AND ((from_user_id = ? AND to_user_id IN ?) OR (to_user_id = ? AND from_user_id IN ?))",
			StatusPending, userID, others, userID, others).
		Find(&reqs).Error
	return reqs, err
}

// handledUpdates 处理申请时要更新的列。handled_at 用数据库的 now()，不用 Go 的 time.Now()
func handledUpdates(status RequestStatus) map[string]any {
	return map[string]any{"status": status, "handled_at": gorm.Expr("now()")}
}

func (r *gormRepository) Accept(ctx context.Context, requestID, toUserID int64) error {
	// Transaction：回调返回 error 就回滚，返回 nil 就提交，作用类似 Spring 的 @Transactional。
	// 回调里所有操作都必须用 tx 而不是 r.db，否则不在同一个事务里。
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var req FriendRequest
		// 条件更新兜住并发：两个标签页同时点"同意"，只有一个能把 pending 改掉。
		// RETURNING 把更新后的整行写回 req，拿到 from_user_id，省一次查询。
		res := tx.Model(&req).
			Clauses(clause.Returning{}).
			Where("id = ? AND to_user_id = ? AND status = ?", requestID, toUserID, StatusPending).
			Updates(handledUpdates(StatusAccepted))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrRequestNotPending
		}

		// ON CONFLICT DO NOTHING：极端并发下双方可能已经是好友（例如旧申请刚被同意时又建了新申请），
		// 这时不应该因为主键冲突让整个事务失败
		friendships := []Friendship{
			{UserID: req.FromUserID, FriendID: req.ToUserID},
			{UserID: req.ToUserID, FriendID: req.FromUserID},
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&friendships).Error
	})
}

func (r *gormRepository) Reject(ctx context.Context, requestID, toUserID int64) error {
	// 只有一条 UPDATE，本身就是原子的，不需要事务
	res := r.db.WithContext(ctx).Model(&FriendRequest{}).
		Where("id = ? AND to_user_id = ? AND status = ?", requestID, toUserID, StatusPending).
		Updates(handledUpdates(StatusRejected))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrRequestNotPending
	}
	return nil
}

func (r *gormRepository) AreFriends(ctx context.Context, a, b int64) (bool, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&Friendship{}).
		Where("user_id = ? AND friend_id = ?", a, b).
		Count(&n).Error
	return n > 0, err
}

func (r *gormRepository) ListFriendships(ctx context.Context, userID int64) ([]Friendship, error) {
	var fs []Friendship
	err := r.db.WithContext(ctx).Where("user_id = ?", userID).Find(&fs).Error
	return fs, err
}

func (r *gormRepository) FilterFriends(ctx context.Context, userID int64, candidates []int64) ([]int64, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	var ids []int64
	// Pluck 只查一列，结果直接写进切片
	err := r.db.WithContext(ctx).Model(&Friendship{}).
		Where("user_id = ? AND friend_id IN ?", userID, candidates).
		Pluck("friend_id", &ids).Error
	return ids, err
}

func (r *gormRepository) DeleteFriendship(ctx context.Context, a, b int64) error {
	// 一条 DELETE 同时删两个方向，单条语句是原子的，不会只删掉一半
	return r.db.WithContext(ctx).
		Where("(user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)", a, b, b, a).
		Delete(&Friendship{}).Error
}
