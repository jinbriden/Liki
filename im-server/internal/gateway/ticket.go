package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

// ErrInvalidTicket 票据不存在、已过期或已被使用
var ErrInvalidTicket = errors.New("invalid websocket ticket")

// TicketStore 一次性 WebSocket 建连票据。
//
// 为什么不直接把 JWT 放进 ws://...?token=xxx：浏览器的 WebSocket API 不能自定义请求头，
// 令牌只能放在 URL 里，而 URL 会被写进访问日志、代理日志、浏览器历史。
// 票据的做法是：前端先用 JWT 调 HTTP 接口换一张票据（有效期很短、只能用一次），再带着票据建连。
// 票据即使被日志记下来，也早已失效，泄露的风险很小。
//
// 第一阶段只部署一个实例，用内存实现；部署多个网关实例时换成 Redis 实现（NFR-10），
// 因为签发票据的实例和建连的实例可能不是同一台。
type TicketStore interface {
	Issue(ctx context.Context, userID int64) (ticket string, expiresAt time.Time, err error)
	// Redeem 兑换票据：成功返回用户 ID，并立即作废这张票据
	Redeem(ctx context.Context, ticket string) (userID int64, err error)
}

type ticketEntry struct {
	userID    int64
	expiresAt time.Time
}

// MemoryTicketStore 内存实现，并发安全
type MemoryTicketStore struct {
	ttl time.Duration
	now func() time.Time // 测试时可以替换

	mu      sync.Mutex
	tickets map[string]ticketEntry
}

func NewMemoryTicketStore(ttl time.Duration) *MemoryTicketStore {
	return &MemoryTicketStore{ttl: ttl, now: time.Now, tickets: map[string]ticketEntry{}}
}

func (s *MemoryTicketStore) Issue(_ context.Context, userID int64) (string, time.Time, error) {
	b := make([]byte, 32) // 256 位随机数，不可猜测
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	ticket := base64.RawURLEncoding.EncodeToString(b) // URL 安全的字符集，可以直接放进查询参数

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	// 顺手清掉过期的票据，防止"只签发不建连"的票据一直占内存。
	// 遍历整个 map 是 O(n)，但票据 30 秒就过期，n 很小；量大以后换 Redis，靠 key 的过期时间自动清理
	for t, e := range s.tickets {
		if !now.Before(e.expiresAt) {
			delete(s.tickets, t)
		}
	}
	expiresAt := now.Add(s.ttl)
	s.tickets[ticket] = ticketEntry{userID: userID, expiresAt: expiresAt}
	return ticket, expiresAt, nil
}

func (s *MemoryTicketStore) Redeem(_ context.Context, ticket string) (int64, error) {
	if ticket == "" {
		return 0, ErrInvalidTicket
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.tickets[ticket]
	if !ok {
		return 0, ErrInvalidTicket
	}
	delete(s.tickets, ticket) // 先删再判断是否过期：无论成功与否，这张票据都不能再用
	if !s.now().Before(e.expiresAt) {
		return 0, ErrInvalidTicket
	}
	return e.userID, nil
}
