// Package gateway WebSocket 长连接网关：建连鉴权、心跳、数据包收发、按用户推送（NFR-08）。
//
// 网关不写业务逻辑：收到的数据包按 type 分发给业务模块注册的处理函数（Handle），
// 业务模块要推送时调用 Push(userID, type, data)，网关只负责把数据送到该用户的所有在线连接（FR-04）。
package gateway

import (
	"context"
	"encoding/json"
)

// ProtocolVersion 当前协议版本。数据包统一带 type 和 version（NFR-09），
// 以后某个 type 的数据格式不兼容地变化时升版本号，旧版本的处理逻辑可以保留。
const ProtocolVersion = 1

// 网关自己处理的数据包类型。业务数据包的类型由各业务模块定义（如 message.send）。
const (
	TypePing  = "ping"  // 客户端 → 服务端：应用层心跳
	TypePong  = "pong"  // 服务端 → 客户端：心跳回复
	TypeError = "error" // 服务端 → 客户端：数据包本身有问题（格式错误、未知类型等）
)

// Packet WebSocket 上传输的数据包（JSON 文本帧）：{"type": "...", "version": 1, "data": {...}}
type Packet struct {
	Type    string          `json:"type"`
	Version int             `json:"version"`
	Data    json.RawMessage `json:"data,omitempty"` // 延迟解析：网关不关心内容，交给对应的处理函数去解析
}

// ErrorData TypeError 数据包的内容。code 沿用 HTTP 接口的错误码（internal/pkg/response）
type ErrorData struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Type string `json:"type,omitempty"` // 出错的那个数据包的 type，便于客户端定位
}

// Session 一个已鉴权的 WebSocket 连接，交给业务处理函数使用
type Session interface {
	// UserID 建连时由票据确定的用户 ID。业务处理函数一律以它为发送方，不信任数据包里的用户 ID（NFR-02）
	UserID() int64
	// Reply 只回复给这一个连接（例如 ACK）。不会阻塞；连接已关闭时直接丢弃
	Reply(ctx context.Context, typ string, data any)
}

// HandlerFunc 业务模块处理某种数据包的函数。
// 同一个连接上的数据包按到达顺序逐个处理（上一个处理完才读下一个），所以同一个标签页发出的消息不会乱序。
// ctx 上已经挂好 conn_id、user_id 日志字段。
type HandlerFunc func(ctx context.Context, s Session, data json.RawMessage)
