import { http } from './client'
import type { Profile } from './friend'

// 以下类型对应后端 internal/message/handler.go 的 resp 结构体

// messageResp：HTTP 接口和 WebSocket 推送共用
export interface Message {
  id: number
  conversationId: number
  seq: number // 会话内递增的序号，用来排序和增量同步
  senderId: number
  recipientId: number
  clientMsgId: string
  type: 'text'
  content: string
  createdAt: string // UTC
}

// chatResp：会话列表中的一项（FR-14）
export interface ChatSummary {
  peer: Profile
  lastMessage: Message | null // M5 一定有；M6 删除消息后可能为 null
  lastSeq: number
  readSeq: number // 我在这个会话里已读到的 seq（FR-15），只增不减
  unread: number // = lastSeq - readSeq
  lastMessageAt: string // UTC，列表按它倒序
}

export interface MessagePage {
  items: Message[] // 按 seq 升序
  hasMore: boolean
}

// 单条消息上限，与后端 MaxContentLen 一致。按字符（Unicode 码点）计数，和 Go 的 rune 一致：
// 用 [...text].length 而不是 text.length，后者按 UTF-16 计数，一个 emoji 会算成 2
export const MAX_MESSAGE_LEN = 1500

export function messageLength(text: string) {
  return [...text].length
}

// 增量同步：seq > afterSeq 的消息。hasMore 为 true 时以最后一条的 seq 继续拉
export function listMessagesAfter(peerId: number, afterSeq: number, limit = 100) {
  return http.get<MessagePage>(`/chats/${peerId}/messages?afterSeq=${afterSeq}&limit=${limit}`)
}

// 向前翻页：seq < beforeSeq 的最近一页；beforeSeq 不传表示最新一页
export function listMessagesBefore(peerId: number, beforeSeq?: number, limit = 30) {
  const q = beforeSeq ? `beforeSeq=${beforeSeq}&` : ''
  return http.get<MessagePage>(`/chats/${peerId}/messages?${q}limit=${limit}`)
}

// 会话列表：只含有消息的会话，按最后一条消息的时间倒序，不分页
export function listChats() {
  return http.get<{ items: ChatSummary[] }>('/chats')
}

// 上报已读位置。服务端只增不减、不超过最新 seq，返回推进后的 readSeq（可能和传入的不同）
export function markRead(peerId: number, seq: number) {
  return http.post<{ readSeq: number }>(`/chats/${peerId}/read`, { seq })
}
