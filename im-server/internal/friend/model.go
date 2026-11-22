package friend

import (
	"errors"
	"time"
)

// 业务哨兵错误，由 service / repository 返回，handler 负责翻译成错误码和提示
var (
	ErrInvalidKeyword        = errors.New("invalid search keyword")
	ErrMessageTooLong        = errors.New("request message too long")
	ErrCannotAddSelf         = errors.New("cannot add self as friend")
	ErrAlreadyFriends        = errors.New("already friends")
	ErrReverseRequestPending = errors.New("the other user has already sent you a request")
	ErrTargetUserNotFound    = errors.New("target user not found")
	// ErrRequestNotFound 申请不存在，或者不是发给当前用户的（不暴露别人的申请是否存在）
	ErrRequestNotFound = errors.New("friend request not found")
	// ErrRequestNotPending 申请已被处理（同意或拒绝），不能再处理
	ErrRequestNotPending = errors.New("friend request already handled")
	// ErrPendingExists 两人之间已有一条待处理申请（不分方向）。
	// 由 repository 在命中唯一索引 uq_friend_requests_pending_pair 时返回，
	// service 据此判断是"同方向重复申请"还是"对方已经申请了我"，不直接返回给 handler。
	ErrPendingExists = errors.New("pending request already exists between the two users")
)

// RequestStatus 好友申请状态，取值和数据库 CHECK 约束一致
type RequestStatus string

const (
	StatusPending  RequestStatus = "pending"
	StatusAccepted RequestStatus = "accepted"
	StatusRejected RequestStatus = "rejected"
)

// Relation 搜索结果中，对方与当前用户的关系
type Relation string

const (
	RelationNone       Relation = "none"        // 可以发送申请
	RelationFriend     Relation = "friend"      // 已是好友
	RelationPendingOut Relation = "pending_out" // 我已申请对方，等待对方处理
	RelationPendingIn  Relation = "pending_in"  // 对方已申请我，我应去申请列表处理
)

// FriendRequest 对应 friend_requests 表
type FriendRequest struct {
	ID         int64 `gorm:"primaryKey"`
	FromUserID int64
	ToUserID   int64
	Message    string
	// Status 零值时 GORM 用 default 标签的值 'pending' 写入（和库里的 DEFAULT 一致）
	Status    RequestStatus `gorm:"default:pending"`
	CreatedAt time.Time     `gorm:"autoCreateTime:false;default:now()"`
	// HandledAt 未处理时为 NULL，所以用指针。处理时用数据库的 now() 写入，不用 time.Now()
	HandledAt *time.Time
}

// Friendship 对应 friendships 表，双向各存一条
type Friendship struct {
	UserID    int64     `gorm:"primaryKey;autoIncrement:false"`
	FriendID  int64     `gorm:"primaryKey;autoIncrement:false"`
	CreatedAt time.Time `gorm:"autoCreateTime:false;default:now()"`
}
