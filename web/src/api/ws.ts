import { http } from './client'

// 对应后端 gateway.ticketResp
export interface WsTicket {
  ticket: string // 一次性票据，30 秒内有效
  expiresAt: string
}

// 用 JWT 换一张一次性的 WebSocket 建连票据。
// 浏览器的 WebSocket 不能带 Authorization 头，令牌只能放进 URL，而 URL 会被写进各种日志；
// 票据用一次就失效，即使出现在日志里也没关系。
export function issueWsTicket() {
  return http.post<WsTicket>('/ws/ticket')
}
