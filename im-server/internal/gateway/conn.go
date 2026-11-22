package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// defaultReadTimeout 多久没收到客户端的任何数据（包括协议层的 pong）就判定连接已死。
	// 服务端每 readTimeout/2 发一次协议层 ping，超时前至少有两次机会收到回复，网络偶尔慢一点不会被误判
	defaultReadTimeout = 60 * time.Second
	// writeTimeout 单次写入的超时：客户端网络卡住时不让写 goroutine 一直阻塞
	writeTimeout = 10 * time.Second
	// maxPacketSize 单个数据包的上限。1500 个字符的消息按 UTF-8 最多 6000 字节，加上 JSON 外壳绰绰有余
	maxPacketSize = 16 << 10
	// sendBuffer 每个连接的发送队列长度。队列满了说明客户端读得太慢，断开它，重连后靠增量同步补齐
	sendBuffer = 64
)

// conn 一个 WebSocket 连接。每个连接两个 goroutine：
//   - readPump 读数据包并交给处理函数（同一连接上的数据包串行处理）；
//   - writePump 从 send 队列取数据写出去。
//
// gorilla/websocket 规定同一时间最多一个 goroutine 写、一个 goroutine 读，
// 所以所有写操作都通过 send 队列交给 writePump，其他 goroutine 不直接写 ws。
type conn struct {
	id          string
	userID      int64
	ws          *websocket.Conn
	gw          *Gateway
	send        chan []byte
	connectedAt time.Time

	// ctx 连接级别的上下文：挂着 conn_id、user_id 日志字段，连接关闭时取消。
	// 不能用握手请求的 ctx：握手的 HTTP handler 一返回，那个 ctx 就被取消了，而连接还要继续用。
	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once
}

func (c *conn) UserID() int64 { return c.userID }

func (c *conn) Reply(ctx context.Context, typ string, data any) {
	b, err := encode(typ, data)
	if err != nil {
		slog.ErrorContext(ctx, "encode ws packet failed", "type", typ, "err", err)
		return
	}
	c.enqueue(b)
}

// enqueue 把数据放进发送队列，不阻塞。返回是否放入成功。
func (c *conn) enqueue(b []byte) bool {
	// select + default 是 Go 里"非阻塞发送"的写法：能放进去就放，放不进去立刻走 default
	select {
	case <-c.ctx.Done():
		return false // 连接已关闭
	case c.send <- b:
		return true
	default:
		// 队列满了：不能阻塞调用方（调用方可能是别的用户的读 goroutine），断开这个慢连接。
		// 另起 goroutine 关闭，因为 close 里会发关闭帧，可能要等一会儿
		go c.close(websocket.CloseTryAgainLater, "send buffer full")
		return false
	}
}

// close 关闭连接，只执行一次（sync.Once）。读、写、推送、服务关闭都可能触发，谁先到谁执行。
func (c *conn) close(code int, reason string) {
	c.closeOnce.Do(func() {
		c.cancel() // 通知 writePump 退出，之后的 enqueue 也会直接丢弃
		c.gw.unregister(c)
		// 尽量发一个关闭帧告诉客户端原因；对方已经断了的话会失败，忽略即可。
		// WriteControl 是 gorilla 允许和其他写操作并发调用的方法
		_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(time.Second))
		_ = c.ws.Close()
		slog.InfoContext(c.ctx, "ws disconnected",
			"reason", reason,
			"duration", time.Since(c.connectedAt).Round(time.Millisecond).String(),
		)
	})
}

func (c *conn) extendReadDeadline() {
	_ = c.ws.SetReadDeadline(time.Now().Add(c.gw.readTimeout))
}

func (c *conn) readPump() {
	defer c.gw.wg.Done()

	c.ws.SetReadLimit(maxPacketSize)
	c.extendReadDeadline()
	// 收到 writePump 发出的协议层 ping 的回复，说明连接是活的
	c.ws.SetPongHandler(func(string) error {
		c.extendReadDeadline()
		return nil
	})

	for {
		msgType, data, err := c.ws.ReadMessage()
		if err != nil {
			c.close(websocket.CloseNormalClosure, readCloseReason(err))
			return
		}
		c.extendReadDeadline() // 收到任何数据包都说明连接是活的，不只是 ping

		if msgType != websocket.TextMessage {
			c.replyError(c.ctx, codeBadRequest, "只支持文本数据包", "")
			continue
		}
		c.gw.dispatch(c, data)
	}
}

// writePump 除了写数据包，还定时发协议层的 ping 帧。
//
// 为什么不能只靠客户端的应用层 ping：浏览器会节流后台标签页的定时器（Chrome 标签页隐藏 5 分钟后，
// 定时器最多每分钟触发一次），客户端 25 秒一次的 ping 会被拖到 60 秒以上，服务端就会误判超时断开。
// 协议层的 ping 由浏览器的网络栈自动回复 pong，不经过 JS，不受节流影响。
// 客户端的应用层 ping 仍然保留，用来让客户端自己发现"服务端已经没了"。
func (c *conn) writePump() {
	defer c.gw.wg.Done()
	ticker := time.NewTicker(c.gw.readTimeout / 2)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
				c.close(websocket.CloseNormalClosure, "ping error: "+err.Error())
				return
			}
		case b := <-c.send:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := c.ws.WriteMessage(websocket.TextMessage, b); err != nil {
				c.close(websocket.CloseNormalClosure, "write error: "+err.Error())
				return
			}
		}
	}
}

func (c *conn) replyError(ctx context.Context, code int, msg, typ string) {
	c.Reply(ctx, TypeError, ErrorData{Code: code, Msg: msg, Type: typ})
}

// readCloseReason 把读错误翻译成便于排查的断开原因
func readCloseReason(err error) string {
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return fmt.Sprintf("client closed (code %d)", ce.Code) // 1000 正常关闭，1001 页面关闭/刷新
	}
	if errors.Is(err, websocket.ErrReadLimit) {
		return "packet too large"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "heartbeat timeout" // readTimeout 内没收到任何数据
	}
	if errors.Is(err, net.ErrClosed) {
		return "closed by server" // 其他地方先关闭了连接（例如服务关闭），读操作随之失败
	}
	return "read error: " + err.Error()
}
