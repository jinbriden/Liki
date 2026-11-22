package ratelimit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"im-server/internal/pkg/response"
)

var ctx = context.Background()

// fakeClock 让令牌桶按测试指定的时间补充令牌，不用真的 sleep
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestMemory(r Rule) (*Memory, *fakeClock) {
	m := NewMemory(r)
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	m.now = clk.now
	return m, clk
}

func allowN(m *Memory, key string, n int) int {
	ok := 0
	for range n {
		if m.Allow(ctx, key) {
			ok++
		}
	}
	return ok
}

func TestMemory_BurstAndRefill(t *testing.T) {
	m, clk := newTestMemory(Rule{Limit: 5, Per: time.Second, Burst: 10})

	if got := allowN(m, "u1", 15); got != 10 {
		t.Errorf("桶满时一次性最多放行 Burst 个：%d, want 10", got)
	}
	clk.advance(200 * time.Millisecond) // 每秒补 5 个 → 200ms 补 1 个
	if got := allowN(m, "u1", 3); got != 1 {
		t.Errorf("200ms 后补充 1 个：%d", got)
	}
	if got := allowN(m, "u2", 3); got != 3 {
		t.Errorf("不同的键互不影响：%d", got)
	}
	clk.advance(time.Hour)
	if got := allowN(m, "u1", 20); got != 10 {
		t.Errorf("补满后最多仍是 Burst 个：%d", got)
	}
}

func TestMemory_BurstDefaultsToLimit(t *testing.T) {
	m, _ := newTestMemory(Rule{Limit: 5, Per: time.Minute})
	if got := allowN(m, "ip", 8); got != 5 {
		t.Errorf("不填 Burst 时等于 Limit：%d", got)
	}
}

func TestMemory_SweepIdleBuckets(t *testing.T) {
	m, clk := newTestMemory(Rule{Limit: 5, Per: time.Minute}) // 从空到满 1 分钟
	allowN(m, "a", 5)
	allowN(m, "b", 1)
	clk.advance(2 * time.Minute)
	m.Allow(ctx, "c") // 触发清理
	if _, ok := m.buckets["a"]; ok || len(m.buckets) != 1 {
		t.Errorf("空闲超过 idle 的桶应被清理：剩 %d 个", len(m.buckets))
	}
	if got := allowN(m, "a", 6); got != 5 {
		t.Errorf("清理后重新来的键是满的桶：%d", got)
	}
}

func newRouter(handlers ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	r.POST("/limited", append(handlers, ok)...)
	r.POST("/free", append(handlers, ok)...)
	return r
}

func do(r *gin.Engine, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
	return w
}

func TestMiddleware_Returns429(t *testing.T) {
	m, _ := newTestMemory(Rule{Limit: 2, Per: time.Minute})
	r := newRouter(Middleware(m, "test", ByIP))

	for i := range 2 {
		if w := do(r, "/limited"); w.Code != http.StatusOK {
			t.Fatalf("第 %d 次应放行：%d", i+1, w.Code)
		}
	}
	w := do(r, "/limited")
	var body response.Body
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusTooManyRequests || body.Code != response.CodeTooManyRequests {
		t.Errorf("超出后应返回 429 / 10004：%d %+v", w.Code, body)
	}
}

func TestRoutes_OnlyMatchingRoute(t *testing.T) {
	m1, _ := newTestMemory(Rule{Limit: 1, Per: time.Minute})
	m2, _ := newTestMemory(Rule{Limit: 5, Per: time.Minute})
	// 同一路由两条规则，任一条不满足就拒绝
	r := newRouter(Routes(map[string][]gin.HandlerFunc{
		"POST /limited": {Middleware(m2, "loose", ByIP), Middleware(m1, "strict", ByIP)},
	}))

	if do(r, "/limited").Code != http.StatusOK || do(r, "/limited").Code != http.StatusTooManyRequests {
		t.Error("/limited 第二次应被严格的规则拒绝")
	}
	for range 5 {
		if w := do(r, "/free"); w.Code != http.StatusOK {
			t.Fatalf("没有规则的路由不受限制：%d", w.Code)
		}
	}
}
