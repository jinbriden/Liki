package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"im-server/internal/pkg/auth"
	"im-server/internal/pkg/logger"
	"im-server/internal/pkg/reqctx"
	"im-server/internal/pkg/response"
)

// 网关回复错误时用的错误码，沿用 HTTP 接口的定义
const (
	codeBadRequest = response.CodeBadRequest
	codeInternal   = response.CodeInternal
)

// Gateway WebSocket 网关。
// 启动阶段由各业务模块调用 Handle 注册数据包处理函数，之后才开始接受连接；运行期间 handlers 只读，不需要加锁。
type Gateway struct {
	tickets  TicketStore
	presence Presence
	upgrader websocket.Upgrader
	handlers map[string]HandlerFunc
	// readTimeout 心跳超时，默认 defaultReadTimeout，测试时改短
	readTimeout time.Duration

	mu     sync.RWMutex
	conns  map[int64]map[*conn]struct{} // 本实例上的连接：userID → 连接集合（一个用户可以多处登录，FR-04）
	closed bool

	wg sync.WaitGroup // 所有连接的读写 goroutine，Close 时等它们退出
}

func New(tickets TicketStore, presence Presence) *Gateway {
	return &Gateway{
		tickets:  tickets,
		presence: presence,
		// CheckOrigin 保持默认：只接受 Origin 和 Host 一致的同源连接，防止别的网站在用户浏览器里偷偷建连
		upgrader:    websocket.Upgrader{},
		handlers:    map[string]HandlerFunc{},
		readTimeout: defaultReadTimeout,
		conns:       map[int64]map[*conn]struct{}{},
	}
}

// Handle 注册某种数据包的处理函数。只能在启动阶段调用；重复注册说明代码写错了，直接 panic。
func (g *Gateway) Handle(typ string, h HandlerFunc) {
	if typ == TypePing || typ == TypePong || typ == TypeError {
		panic("gateway: packet type " + typ + " is reserved")
	}
	if _, dup := g.handlers[typ]; dup {
		panic("gateway: duplicate handler for packet type " + typ)
	}
	g.handlers[typ] = h
}

// RegisterRoutes 注册网关的 HTTP 路由：
//   - POST /ws/ticket（需要登录）：用 JWT 换一张一次性建连票据；
//   - GET  /ws?ticket=...（公开，靠票据鉴权）：升级为 WebSocket。
func (g *Gateway) RegisterRoutes(public, protected *gin.RouterGroup) {
	protected.POST("/ws/ticket", g.issueTicket)
	public.GET("/ws", g.serveWS)
}

type ticketResp struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (g *Gateway) issueTicket(c *gin.Context) {
	ctx := c.Request.Context()
	ticket, expiresAt, err := g.tickets.Issue(ctx, auth.UserID(c))
	if err != nil {
		slog.ErrorContext(ctx, "issue ws ticket failed", "err", err)
		response.Fail(c, http.StatusInternalServerError, response.CodeInternal, "服务器内部错误")
		return
	}
	slog.DebugContext(ctx, "ws ticket issued") // 不记录票据本身
	response.OK(c, ticketResp{Ticket: ticket, ExpiresAt: expiresAt.UTC()})
}

func (g *Gateway) serveWS(c *gin.Context) {
	ctx := c.Request.Context()
	userID, err := g.tickets.Redeem(ctx, c.Query("ticket"))
	if err != nil {
		// 握手阶段就拒绝（NFR-02），还没升级成 WebSocket，可以正常返回 HTTP 401
		slog.WarnContext(ctx, "ws handshake rejected", "reason", "invalid or expired ticket")
		response.Fail(c, http.StatusUnauthorized, response.CodeUnauthorized, "未登录或登录已过期")
		return
	}

	ws, err := g.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade 失败时已经自己写好了 HTTP 错误响应（如 400、403 跨域）
		slog.WarnContext(ctx, "ws upgrade failed", "user_id", userID, "err", err)
		return
	}

	id := newConnID()
	connCtx, cancel := context.WithCancel(logger.WithAttrs(context.Background(), "conn_id", id, "user_id", userID))
	cn := &conn{
		id:          id,
		userID:      userID,
		ws:          ws,
		gw:          g,
		send:        make(chan []byte, sendBuffer),
		connectedAt: time.Now(),
		ctx:         connCtx,
		cancel:      cancel,
	}
	n, ok := g.register(cn)
	if !ok {
		cn.close(websocket.CloseGoingAway, "server shutting down")
		return
	}
	slog.InfoContext(connCtx, "ws connected", "user_conns", n, "client_ip", c.ClientIP())

	g.wg.Add(2)
	go cn.writePump()
	go cn.readPump()
	// handler 直接返回即可，连接已经被 Upgrade 接管（Hijack），由上面两个 goroutine 负责
}

// register 登记连接，返回该用户在本实例上的连接数。网关已关闭时返回 false。
func (g *Gateway) register(c *conn) (int, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return 0, false
	}
	set, ok := g.conns[c.userID]
	if !ok {
		set = map[*conn]struct{}{}
		g.conns[c.userID] = set
	}
	set[c] = struct{}{}
	g.presence.Online(c.ctx, c.userID, c.id)
	return len(set), true
}

func (g *Gateway) unregister(c *conn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	set := g.conns[c.userID]
	if _, ok := set[c]; !ok {
		return
	}
	delete(set, c)
	if len(set) == 0 {
		delete(g.conns, c.userID)
	}
	g.presence.Offline(c.ctx, c.userID, c.id)
}

// Push 把一个数据包推送给 userID 的所有在线连接（多个标签页、多台设备，FR-04）。
// 不阻塞、不返回错误：用户离线或某个连接已断开时直接跳过，客户端重连后会按 seq 增量同步补齐。
func (g *Gateway) Push(ctx context.Context, userID int64, typ string, data any) {
	if !g.presence.IsOnline(ctx, userID) {
		slog.DebugContext(ctx, "ws push skipped: user offline", "type", typ, "target_user_id", userID)
		return
	}
	b, err := encode(typ, data)
	if err != nil {
		slog.ErrorContext(ctx, "encode ws packet failed", "type", typ, "err", err)
		return
	}

	// 只在锁内复制一份连接列表，发送放到锁外：enqueue 在队列满时会关闭连接，而关闭要拿写锁，
	// 在持有读锁时调用会死锁
	g.mu.RLock()
	targets := make([]*conn, 0, len(g.conns[userID]))
	for c := range g.conns[userID] {
		targets = append(targets, c)
	}
	g.mu.RUnlock()

	delivered := 0
	for _, c := range targets {
		if c.enqueue(b) {
			delivered++
		}
	}
	slog.DebugContext(ctx, "ws push", "type", typ, "target_user_id", userID, "conns", len(targets), "delivered", delivered)
}

// Close 关闭所有连接并等待读写 goroutine 退出。服务关闭时调用，之后不再接受新连接。
func (g *Gateway) Close() {
	g.mu.Lock()
	g.closed = true
	var all []*conn
	for _, set := range g.conns {
		for c := range set {
			all = append(all, c)
		}
	}
	g.mu.Unlock()

	for _, c := range all {
		c.close(websocket.CloseGoingAway, "server shutdown") // 1001：客户端收到后会自动重连
	}
	g.wg.Wait()
}

// dispatch 解析数据包并交给对应的处理函数，在连接的读 goroutine 里调用
func (g *Gateway) dispatch(c *conn, raw []byte) {
	ctx := c.ctx
	var p Packet
	if err := json.Unmarshal(raw, &p); err != nil || p.Type == "" {
		slog.WarnContext(ctx, "ws bad packet", "err", err, "size", len(raw)) // 不记录原文，里面可能有消息正文
		c.replyError(ctx, codeBadRequest, "数据包格式错误", "")
		return
	}
	if p.Version != ProtocolVersion {
		slog.WarnContext(ctx, "ws unsupported protocol version", "type", p.Type, "version", p.Version)
		c.replyError(ctx, codeBadRequest, "不支持的协议版本", p.Type)
		return
	}
	if p.Type == TypePing {
		c.Reply(ctx, TypePong, nil) // 心跳很频繁，不打日志
		return
	}

	h, ok := g.handlers[p.Type]
	if !ok {
		slog.WarnContext(ctx, "ws unknown packet type", "type", p.Type)
		c.replyError(ctx, codeBadRequest, "不支持的数据包类型", p.Type)
		return
	}

	slog.DebugContext(ctx, "ws packet received", "type", p.Type)
	// 处理函数用脱离连接的 ctx（保留 conn_id、user_id 日志字段）：连接在处理途中被关闭（写失败、服务端关闭）时，
	// 已经开始的写操作照样完成并推送给其他人，不会出现"消息已写库、却因为 ctx 取消而报错、推送被跳过"。
	// 回复本身不受影响：Reply 按连接自己的状态判断能不能发
	hctx, cancel := reqctx.Detach(ctx)
	defer cancel()
	defer func() {
		// 某个处理函数 panic 不能拖垮整个连接（更不能拖垮进程），和 HTTP 的 Recovery 中间件同理
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "ws handler panic", "type", p.Type, "panic", r)
			c.replyError(ctx, codeInternal, "服务器内部错误", p.Type)
		}
	}()
	h(hctx, c, p.Data)
}

// encode 把 type 和 data 包装成统一格式的数据包
func encode(typ string, data any) ([]byte, error) {
	b, err := json.Marshal(struct {
		Type    string `json:"type"`
		Version int    `json:"version"`
		Data    any    `json:"data,omitempty"`
	}{typ, ProtocolVersion, data})
	if err != nil {
		return nil, fmt.Errorf("encode packet %s: %w", typ, err)
	}
	return b, nil
}

func newConnID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
