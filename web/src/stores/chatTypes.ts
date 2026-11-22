// 聊天相关的共用类型。单独放一个文件，让 chat store、会话列表 store 和本地缓存都能引用，不形成循环依赖

export type MessageStatus = 'sending' | 'sent' | 'failed'

export interface ChatMessage {
  // `${senderId}:${clientMsgId}`：同一条消息从"发送中"变成"已发送"时 key 不变，React 不会重建这一行
  key: string
  clientMsgId: string
  id?: number // 服务端确认后才有
  seq?: number // 服务端确认后才有
  senderId: number
  recipientId: number
  content: string
  createdAt: string
  status: MessageStatus
  error?: string // 发送失败的原因，显示在消息下方
  retryable?: boolean // 能否点击重试（消息过长、非好友等被服务端明确拒绝的不能重试）
}
