// Package ratelimit 接口限流（NFR-05）：按 IP 或用户限制请求频率，超出时返回 10004（HTTP 429）。
//
// 算法是令牌桶：每个键（IP、用户 ID）一个桶，容量 Burst 个令牌，按 Limit / Per 的速度补充，每次请求消耗一个，
// 桶空了就拒绝。所以"平均每 Per 最多 Limit 次，允许一次性用掉 Burst 次"。
//
// Limiter 是接口（和网关的在线状态一样，NFR-10）：第一阶段用内存实现，只在单个进程内有效；
// 以后部署多台时换成 Redis 实现，调用方不用改。
package ratelimit

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"

	"im-server/internal/pkg/auth"
	"im-server/internal/pkg/response"
)

// Limiter 判断 key 这次请求是否放行（放行时消耗一个令牌）。
// 不返回错误：限流器自己出故障（以后的 Redis 实现连不上）时应当放行并记日志，不能因此让正常请求全部失败。
type Limiter interface {
	Allow(ctx context.Context, key string) bool
}

// Rule 一条限流规则：平均每 Per 最多 Limit 次，最多一次性突发 Burst 次（不填时等于 Limit）
type Rule struct {
	Name  string // 用于日志，如 "login_ip"
	Limit int
	Per   time.Duration
	Burst int
}

// Memory 内存里的令牌桶，每个键一个。用 golang.org/x/time/rate 的 Limiter 实现单个桶。
type Memory struct {
	name  string
	limit rate.Limit
	burst int
	idle  time.Duration // 多久没用的桶可以丢掉

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
	now       func() time.Time // 测试时替换成假时钟
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

func NewMemory(r Rule) *Memory {
	burst := r.Burst
	if burst <= 0 {
		burst = r.Limit
	}
	limit := rate.Limit(float64(r.Limit) / r.Per.Seconds()) // 每秒补充几个令牌
	// 一个桶空闲超过"从空到满"的时间后，和新建的桶完全一样，丢掉不影响结果，只是省内存
	idle := max(time.Duration(float64(burst)/float64(limit)*float64(time.Second)), time.Minute)
	return &Memory{name: r.Name, limit: limit, burst: burst, idle: idle, buckets: map[string]*bucket{}, now: time.Now}
}

func (m *Memory) Allow(_ context.Context, key string) bool {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()

	// 顺带清理空闲的桶，不另起后台 goroutine：每过 idle 时间才扫一次，均摊下来开销很小。
	// 否则每来一个新 IP 都留下一个桶，内存只增不减
	if now.Sub(m.lastSweep) > m.idle {
		for k, b := range m.buckets {
			if now.Sub(b.seen) > m.idle {
				delete(m.buckets, k) // Go 允许在 range map 的过程中删除元素
			}
		}
		m.lastSweep = now
	}

	b, ok := m.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(m.limit, m.burst)}
		m.buckets[key] = b
	}
	b.seen = now
	return b.lim.AllowN(now, 1)
}

// KeyFunc 从请求里取限流的键
type KeyFunc func(c *gin.Context) string

// ByIP 按客户端 IP。依赖 gin 的 SetTrustedProxies 配置：不信任任何代理时就是 TCP 连接的对端地址，
// 不会被客户端伪造的 X-Forwarded-For 绕过（见 main.go）
func ByIP(c *gin.Context) string { return c.ClientIP() }

// ByUser 按当前登录用户。只能用在鉴权中间件之后
func ByUser(c *gin.Context) string { return strconv.FormatInt(auth.UserID(c), 10) }

const msgTooManyRequests = "操作太频繁，请稍后再试"

// Middleware 超出频率时直接返回 429 / 10004，不再执行后面的 handler
func Middleware(l Limiter, name string, key KeyFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if l.Allow(c.Request.Context(), key(c)) {
			c.Next()
			return
		}
		// 不记录键本身：按 IP 限流时它是 IP，访问日志里已经有 client_ip；按用户时 user_id 已在 ctx 上
		slog.WarnContext(c.Request.Context(), "rate limited", "rule", name)
		response.Fail(c, http.StatusTooManyRequests, response.CodeTooManyRequests, msgTooManyRequests)
		c.Abort()
	}
}

// Routes 按路由挑选限流中间件：键是 "方法 路由模板"，如 "POST /api/v1/auth/login"。
// 作为路由组的中间件使用，这样各业务模块注册路由时不用关心限流，规则集中在 main.go 里配置。
// 一个路由可以有多条规则（例如登录既按 IP 又按账号），依次检查。
func Routes(rules map[string][]gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		for _, h := range rules[c.Request.Method+" "+c.FullPath()] {
			h(c)
			if c.IsAborted() {
				return
			}
		}
		c.Next()
	}
}
