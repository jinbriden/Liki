package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"im-server/internal/pkg/response"
)

var ctx = context.Background()

func TestMemoryTicketStore(t *testing.T) {
	s := NewMemoryTicketStore(30 * time.Second)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	t1, exp, err := s.Issue(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(now.Add(30*time.Second)) || len(t1) != 43 {
		t.Errorf("expiresAt = %v, len(ticket) = %d", exp, len(t1))
	}
	if uid, err := s.Redeem(ctx, t1); err != nil || uid != 42 {
		t.Fatalf("Redeem = %d, %v; want 42", uid, err)
	}
	// 只能用一次
	if _, err := s.Redeem(ctx, t1); !errors.Is(err, ErrInvalidTicket) {
		t.Errorf("第二次兑换：err = %v, want ErrInvalidTicket", err)
	}

	// 过期
	t2, _, _ := s.Issue(ctx, 42)
	now = now.Add(30 * time.Second)
	if _, err := s.Redeem(ctx, t2); !errors.Is(err, ErrInvalidTicket) {
		t.Errorf("过期票据：err = %v, want ErrInvalidTicket", err)
	}
	if _, err := s.Redeem(ctx, "not-a-ticket"); !errors.Is(err, ErrInvalidTicket) {
		t.Errorf("伪造票据：err = %v", err)
	}
}

// ---------- 端到端：真实的 HTTP 服务 + WebSocket 客户端 ----------

type testServer struct {
	gw      *Gateway
	tickets *MemoryTicketStore
	url     string // ws://.../ws
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	gin.SetMode(gin.TestMode)
	tickets := NewMemoryTicketStore(30 * time.Second)
	gw := New(tickets, NewMemoryPresence())

	// 业务处理函数示例：echo 原样回复，broadcast 推送给自己的所有连接，boom 模拟 panic
	gw.Handle("test.echo", func(ctx context.Context, s Session, data json.RawMessage) {
		s.Reply(ctx, "test.echo", data)
	})
	gw.Handle("test.broadcast", func(ctx context.Context, s Session, data json.RawMessage) {
		gw.Push(ctx, s.UserID(), "test.broadcast", data)
	})
	gw.Handle("test.boom", func(context.Context, Session, json.RawMessage) { panic("boom") })

	r := gin.New()
	api := r.Group("")
	gw.RegisterRoutes(api, api) // 测试里不挂鉴权中间件，票据直接从 store 签发
	srv := httptest.NewServer(r)
	t.Cleanup(func() {
		gw.Close()
		srv.Close()
	})
	return &testServer{gw: gw, tickets: tickets, url: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"}
}

func (s *testServer) dial(t *testing.T, userID int64) *websocket.Conn {
	t.Helper()
	ticket, _, err := s.tickets.Issue(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	ws, resp, err := websocket.DefaultDialer.Dial(s.url+"?ticket="+ticket, nil)
	if err != nil {
		t.Fatalf("dial: %v (status %v)", err, resp)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}

func send(t *testing.T, ws *websocket.Conn, raw string) {
	t.Helper()
	if err := ws.WriteMessage(websocket.TextMessage, []byte(raw)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func recv(t *testing.T, ws *websocket.Conn) Packet {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	var p Packet
	if err := ws.ReadJSON(&p); err != nil {
		t.Fatalf("read: %v", err)
	}
	return p
}

func TestGateway_Handshake(t *testing.T) {
	s := newTestServer(t)

	for name, query := range map[string]string{"没有票据": "", "伪造票据": "?ticket=abc"} {
		_, resp, err := websocket.DefaultDialer.Dial(s.url+query, nil)
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s：应在握手阶段返回 401，got err=%v resp=%v", name, err, resp)
		}
	}

	// 票据用过一次就失效
	ticket, _, _ := s.tickets.Issue(ctx, 1)
	ws, _, err := websocket.DefaultDialer.Dial(s.url+"?ticket="+ticket, nil)
	if err != nil {
		t.Fatalf("第一次建连：%v", err)
	}
	defer ws.Close()
	_, resp, err := websocket.DefaultDialer.Dial(s.url+"?ticket="+ticket, nil)
	if err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("票据重复使用应返回 401，got %v", resp)
	}
}

func TestGateway_PingAndDispatch(t *testing.T) {
	s := newTestServer(t)
	ws := s.dial(t, 1)

	send(t, ws, `{"type":"ping","version":1}`)
	if p := recv(t, ws); p.Type != TypePong {
		t.Errorf("ping → %s, want pong", p.Type)
	}

	send(t, ws, `{"type":"test.echo","version":1,"data":{"x":1}}`)
	if p := recv(t, ws); p.Type != "test.echo" || string(p.Data) != `{"x":1}` {
		t.Errorf("echo → %+v", p)
	}

	// 出错的数据包回复 error，但不断开连接
	cases := []struct {
		raw      string
		wantCode int
		wantMsg  string
	}{
		{`not json`, response.CodeBadRequest, "数据包格式错误"},
		{`{"version":1}`, response.CodeBadRequest, "数据包格式错误"},
		{`{"type":"test.echo","version":2}`, response.CodeBadRequest, "不支持的协议版本"},
		{`{"type":"nope","version":1}`, response.CodeBadRequest, "不支持的数据包类型"},
		{`{"type":"test.boom","version":1}`, response.CodeInternal, "服务器内部错误"},
	}
	for _, c := range cases {
		send(t, ws, c.raw)
		p := recv(t, ws)
		var e ErrorData
		_ = json.Unmarshal(p.Data, &e)
		if p.Type != TypeError || e.Code != c.wantCode || e.Msg != c.wantMsg {
			t.Errorf("%s → %s %+v, want error %d %q", c.raw, p.Type, e, c.wantCode, c.wantMsg)
		}
	}

	send(t, ws, `{"type":"ping","version":1}`)
	if p := recv(t, ws); p.Type != TypePong {
		t.Error("出错之后连接应仍可用")
	}
}

func TestGateway_PushToAllConnections(t *testing.T) {
	s := newTestServer(t)
	tab1 := s.dial(t, 1)
	tab2 := s.dial(t, 1) // 同一个用户的第二个标签页
	other := s.dial(t, 2)

	send(t, tab1, `{"type":"test.broadcast","version":1,"data":"hi"}`)
	for i, ws := range []*websocket.Conn{tab1, tab2} {
		if p := recv(t, ws); p.Type != "test.broadcast" || string(p.Data) != `"hi"` {
			t.Errorf("tab%d 收到 %+v", i+1, p)
		}
	}

	// 其他用户收不到：发一个 ping，第一个回来的应该是 pong
	send(t, other, `{"type":"ping","version":1}`)
	if p := recv(t, other); p.Type != TypePong {
		t.Errorf("其他用户收到了 %s", p.Type)
	}

	// 一个标签页关闭后，推送只到剩下的连接
	tab2.Close()
	waitFor(t, func() bool { return s.connCount(1) == 1 })
	s.gw.Push(ctx, 1, "test.direct", nil)
	if p := recv(t, tab1); p.Type != "test.direct" {
		t.Errorf("tab1 收到 %s", p.Type)
	}

	// 全部断开后用户离线，推送直接跳过
	tab1.Close()
	waitFor(t, func() bool { return !s.gw.presence.IsOnline(ctx, 1) })
}

func TestGateway_HeartbeatTimeout(t *testing.T) {
	s := newTestServer(t)
	s.gw.readTimeout = 300 * time.Millisecond
	alive := s.dial(t, 1)
	dead := s.dial(t, 2) // 模拟断网：连接还在，但客户端不再发任何数据

	// alive 按时发 ping，超过 readTimeout 也不会被断开
	for range 4 {
		time.Sleep(150 * time.Millisecond)
		send(t, alive, `{"type":"ping","version":1}`)
		if p := recv(t, alive); p.Type != TypePong {
			t.Fatalf("got %s, want pong", p.Type)
		}
	}

	// dead 早已超时：它不读数据，也就不会回复服务端的协议层 ping（gorilla 只在读的时候处理控制帧），
	// 服务端断开并清理掉它，用户 2 变为离线
	waitFor(t, func() bool { return !s.gw.presence.IsOnline(ctx, 2) })
	_ = dead.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := dead.ReadMessage(); err == nil {
		t.Error("超时的连接应已被服务端关闭")
	}
	if s.connCount(1) != 1 {
		t.Error("按时心跳的连接不应被断开")
	}
}

// 模拟浏览器后台标签页：应用层 ping 被节流、一次都不发，但网络栈会自动回复协议层 ping，连接不应被断开
func TestGateway_ProtocolPingKeepsIdleClientAlive(t *testing.T) {
	s := newTestServer(t)
	s.gw.readTimeout = 300 * time.Millisecond
	idle := s.dial(t, 1)

	// 后台持续读取：gorilla 客户端在读的过程中自动回复服务端的 ping（相当于浏览器网络栈）
	done := make(chan error, 1)
	go func() {
		_, _, err := idle.ReadMessage()
		done <- err
	}()

	time.Sleep(time.Second) // 超过 readTimeout 三倍以上，期间客户端没有发任何应用层数据
	select {
	case err := <-done:
		t.Fatalf("空闲但会回复 pong 的连接被断开了：%v", err)
	default:
	}
	if s.connCount(1) != 1 {
		t.Error("连接应仍然在线")
	}
}

func TestGateway_CloseDisconnectsClients(t *testing.T) {
	s := newTestServer(t)
	ws := s.dial(t, 1)
	s.gw.Close()

	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := ws.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseGoingAway) {
		t.Errorf("服务关闭时客户端应收到 1001，got %v", err)
	}
	// 关闭后不再接受新连接
	ticket, _, _ := s.tickets.Issue(ctx, 1)
	if ws2, _, err := websocket.DefaultDialer.Dial(s.url+"?ticket="+ticket, nil); err == nil {
		_ = ws2.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, _, err := ws2.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseGoingAway) {
			t.Errorf("网关关闭后新连接应立即被关闭，got %v", err)
		}
		ws2.Close()
	}
}

func (s *testServer) connCount(userID int64) int {
	s.gw.mu.RLock()
	defer s.gw.mu.RUnlock()
	return len(s.gw.conns[userID])
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
