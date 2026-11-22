package gateway

import (
	"context"
	"sync"
)

// Presence 在线状态：用户有没有在线连接（NFR-10）。
//
// 第一阶段只有一个网关实例，用内存实现。以后网关部署多台时换成 Redis 实现，
// 记录的就不只是"在不在线"，还有"连在哪几个实例上"，推送时按它把消息转发到对应实例，业务代码不用改。
type Presence interface {
	Online(ctx context.Context, userID int64, connID string)
	Offline(ctx context.Context, userID int64, connID string)
	IsOnline(ctx context.Context, userID int64) bool
}

// MemoryPresence 内存实现：userID → 该用户的在线连接 ID 集合
type MemoryPresence struct {
	mu    sync.RWMutex
	conns map[int64]map[string]struct{} // map[string]struct{} 是 Go 里集合（Set）的惯用写法，struct{} 不占内存
}

func NewMemoryPresence() *MemoryPresence {
	return &MemoryPresence{conns: map[int64]map[string]struct{}{}}
}

func (p *MemoryPresence) Online(_ context.Context, userID int64, connID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	set, ok := p.conns[userID]
	if !ok {
		set = map[string]struct{}{}
		p.conns[userID] = set
	}
	set[connID] = struct{}{}
}

func (p *MemoryPresence) Offline(_ context.Context, userID int64, connID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	set := p.conns[userID]
	delete(set, connID)
	if len(set) == 0 {
		delete(p.conns, userID)
	}
}

func (p *MemoryPresence) IsOnline(_ context.Context, userID int64) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.conns[userID]) > 0
}
