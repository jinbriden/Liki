// Package reqctx 让"已经开始执行的写操作"不因为客户端断开而半途而废。
//
// 问题：net/http 在客户端断开（关掉页面、跳转、网络中断）时取消请求的 ctx。
// 如果这时正好在提交事务——COMMIT 已经发到数据库、还没收到确认——数据库其实提交成功了，
// 驱动却只能报告 context canceled。于是接口返回 500，提交之后的实时推送也被跳过：
// 例如同意好友申请时，好友关系已经建好，申请方却收不到通知、自己的其他标签页也不同步。
//
// 做法：写操作改用 context.WithoutCancel 派生的 ctx——保留父 ctx 上的值（request_id、user_id 等日志字段），
// 但不再跟着父 ctx 取消；再加一个超时兜底，避免卡住的数据库调用永远不结束。
// 读操作（GET）不处理：客户端走了，查询结果也没人要，取消正好省掉白做的工作。
//
// 类比 Java：Servlet 容器在客户端断开时不会中断正在执行的 @Transactional 方法，这里是让 Go 也这样做。
package reqctx

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// WriteTimeout 写操作脱离请求之后最多再执行多久。正常的写操作都在毫秒级，这只是兜底
const WriteTimeout = 30 * time.Second

// Detach 返回一个不随 parent 取消、但保留 parent 上所有值的 ctx，并带 WriteTimeout 超时。用完必须调用 cancel。
func Detach(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), WriteTimeout)
}

// Middleware 对写请求（GET / HEAD / OPTIONS 以外的方法）把 c.Request 的 ctx 换成 Detach 后的 ctx。
// handler 一律通过 c.Request.Context() 取 ctx，所以放在路由前面即可全局生效。
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		ctx, cancel := Detach(c.Request.Context())
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
