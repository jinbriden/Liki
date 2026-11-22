package reqctx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

type ctxKey struct{}

// runWithCanceledClient 模拟"客户端已经断开"：请求的 ctx 在进入 handler 之前就被取消了。
// 返回 handler 看到的 ctx 是否已取消、ctx 上的值是否还在、是否带超时。
func runWithCanceledClient(t *testing.T, method string) (canceled, hasValue, hasDeadline bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware())
	r.Handle(method, "/x", func(c *gin.Context) {
		ctx := c.Request.Context()
		canceled = ctx.Err() != nil
		hasValue = ctx.Value(ctxKey{}) == "req-1"
		_, hasDeadline = ctx.Deadline()
		c.Status(http.StatusOK)
	})

	parent, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "req-1"))
	cancel()
	req := httptest.NewRequest(method, "/x", nil).WithContext(parent)
	r.ServeHTTP(httptest.NewRecorder(), req)
	return
}

func TestMiddleware_WriteRequestNotCanceled(t *testing.T) {
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		canceled, hasValue, hasDeadline := runWithCanceledClient(t, m)
		if canceled {
			t.Errorf("%s：客户端断开后写操作的 ctx 不应被取消", m)
		}
		if !hasValue {
			t.Errorf("%s：应保留父 ctx 上的值（request_id、user_id 等）", m)
		}
		if !hasDeadline {
			t.Errorf("%s：应带超时兜底", m)
		}
	}
}

func TestMiddleware_ReadRequestStillCanceled(t *testing.T) {
	canceled, _, _ := runWithCanceledClient(t, http.MethodGet)
	if !canceled {
		t.Error("GET：客户端断开后应照常取消，省掉没人要的查询")
	}
}
