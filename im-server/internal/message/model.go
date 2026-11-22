package message

import (
	"errors"
	"time"
)

// 业务哨兵错误，由 service / repository 返回，handler（HTTP 和 WebSocket）负责翻译成错误码和提示
var (
	ErrEmptyContent       = errors.New("message content is empty")
	ErrContentTooLong     = errors.New("message content too long")
	ErrInvalidContent     = errors.New("message content contains unsupported characters")
	ErrInvalidClientMsgID = errors.New("invalid client message id")
	ErrInvalidPeer        = errors.New("invalid peer user")
	ErrInvalidReadSeq     = errors.New("invalid read seq")
	// ErrStrangerLimit 非好友之间，会话里上一条消息也是自己发的（FR-12：对方回复前只能发一条）。
	// 由 repository 在分配 seq 的事务里判断后返回，事务回滚、不占用 seq
	ErrStrangerLimit = errors.New("stranger message limit reached")
	// ErrInvalidDeleteCount 一次删除的条数不在 1..MaxDeleteBatch 之间（FR-19）
	ErrInvalidDeleteCount = errors.New("invalid delete count")
	// ErrInvalidSeq 要删除的消息不存在：seq 不在 1..last_seq 之间，或者根本没有这个会话
	ErrInvalidSeq = errors.New("invalid message seq")
	// ErrRateLimited 发消息太频繁（NFR-05）
	ErrRateLimited = errors.New("send rate limited")
	// ErrInvalidClearSeq 清空位置为负数，属于客户端代码的 bug
	ErrInvalidClearSeq = errors.New("invalid clear seq")

	ErrConversationNotFound = errors.New("conversation not found")
	ErrMessageNotFound      = errors.New("message not found")
	// ErrDuplicateMessage (sender_id, client_msg_id) 已存在，即同一条消息被重复提交。
	// 由 repository 在命中唯一约束时返回，service 据此查出已有的那条消息返回（去重），不直接返回给客户端。
	ErrDuplicateMessage = errors.New("duplicate client message id")
)

// TypeText 消息类型。第一阶段只有文字消息，取值和库里的 CHECK 约束一致
const TypeText = "text"

// Conversation 对应 conversations 表。UserA < UserB。
type Conversation struct {
	ID            int64 `gorm:"primaryKey"`
	UserA         int64
	UserB         int64
	LastSeq       int64
	LastMessageAt *time.Time
	CreatedAt     time.Time `gorm:"autoCreateTime:false;default:now()"`
}

// peerOf 单聊会话里 userID 的对方
func (c *Conversation) peerOf(userID int64) int64 {
	if c.UserA == userID {
		return c.UserB
	}
	return c.UserA
}

// ConversationMember 对应 conversation_members 表
type ConversationMember struct {
	ConversationID int64 `gorm:"primaryKey;autoIncrement:false"`
	UserID         int64 `gorm:"primaryKey;autoIncrement:false"`
	ReadSeq        int64
	ClearedSeq     int64 // 清空聊天记录的位置，seq <= 它的消息对该成员不可见（FR-19）
}

// Deletion 删除记录同步（GET /message-deletions）的一项：第几号删除记录、在和谁的会话里、删的是哪个 seq
type Deletion struct {
	ID     int64 // message_deletions.id，作为增量同步的游标
	PeerID int64
	Seq    int64
}

// Message 对应 messages 表
type Message struct {
	ID             int64 `gorm:"primaryKey"`
	ConversationID int64
	Seq            int64
	SenderID       int64
	ClientMsgID    string
	Type           string `gorm:"default:text"`
	Content        string
	CreatedAt      time.Time `gorm:"autoCreateTime:false;default:now()"`

	// RecipientID 接收方，不是表里的列（gorm:"-"），由 service 根据会话双方填上，方便推送和接口输出
	RecipientID int64 `gorm:"-"`
}

// ConversationSummary 会话列表中的一项在 repository 层的样子：会话本身、我的已读和清空位置、我能看到的最后一条消息。
// 由 ListConversations 一次查出来，service 再补上对方的资料。
// LastMessage 是排除了我删除、清空的消息之后的最后一条，全部删光时为 nil（FR-14）。
type ConversationSummary struct {
	Conversation Conversation
	ReadSeq      int64
	ClearedSeq   int64
	LastMessage  *Message
}

// pairOf 把两个用户 ID 排成 (较小, 较大)，对应 conversations 表的 (user_a, user_b)
func pairOf(a, b int64) (int64, int64) {
	return min(a, b), max(a, b)
}
