import { create } from 'zustand'
import { errorMessage } from '../api/client'
import type { Profile } from '../api/friend'
import { listChats, type ChatSummary, type Message } from '../api/message'
import { loadChatSnapshot, saveChats } from '../db/chatCache'
import { socket } from '../realtime/socket'
import { useAuthStore } from './auth'
import { useFriendStore } from './friend'

// 会话列表（FR-14）和每个会话的未读数（FR-15），按对方用户 ID 索引。
//
// 数据来源有三个，合并时一律"只往前走"：lastSeq、readSeq 都取较大值。
//   1. 本地快照（IndexedDB）：打开页面时先显示；
//   2. 服务端 GET /chats：登录、重连时拉取，是准的；
//   3. 实时推送：message.new / message.ack 更新最后一条消息，chat.read 更新已读位置。
// 未读数 = lastSeq - readSeq。自己发消息时服务端会把自己的 readSeq 推进到这条消息（发送即已读），
// 本地收到自己发的消息时同样推进，所以自己发的消息不会算进未读。
// clearedSeq（清空位置，FR-19）同样只往前走；chat store 订阅它的变化，删除本地缓存里被清空的消息。

interface ChatListState {
  chats: Record<number, ChatSummary>
  loaded: boolean // 至少从服务端成功拉取过一次
  error: string | null
  // 还没有会话、也不是好友的人的资料（从搜索结果点"发消息"进来时记下，FR-12）。
  // 只在内存里，用于临时项和对话框头部显示用户名；发出第一条消息后会话列表会带上资料，以列表为准
  peers: Record<number, Profile>

  load: () => Promise<void>
  loadSnapshot: () => Promise<void>
  rememberPeer: (p: Profile) => void
}

export const unreadOf = (c: ChatSummary | undefined) => (c ? Math.max(0, c.lastSeq - c.readSeq) : 0)

// selectTotalUnread 所有会话的未读总数，用于导航栏角标（FR-14）
export const selectTotalUnread = (s: ChatListState) => {
  let n = 0
  for (const c of Object.values(s.chats)) n += unreadOf(c)
  return n
}

const myId = () => useAuthStore.getState().user?.id ?? 0
const currentToken = () => useAuthStore.getState().token

// merge 合并同一个会话的两份数据：最后一条消息取 seq 较大的（一样大时以 b 为准），已读、清空位置取较大的（但不超过最新 seq）
function merge(a: ChatSummary | undefined, b: ChatSummary): ChatSummary {
  if (!a) return normalize(b)
  const newer = b.lastSeq >= a.lastSeq ? b : a
  return normalize({
    ...newer,
    peer: b.peer.username ? b.peer : a.peer, // 推送临时建出来的会话没有用户名，以有的为准
    readSeq: Math.max(a.readSeq, b.readSeq),
    clearedSeq: Math.max(a.clearedSeq ?? 0, b.clearedSeq ?? 0),
  })
}

function normalize(c: ChatSummary): ChatSummary {
  const readSeq = Math.min(c.readSeq, c.lastSeq)
  const clearedSeq = Math.min(c.clearedSeq ?? 0, c.lastSeq)
  // 摘要里的最后一条已经被清空掉了：不再显示
  const lastMessage = c.lastMessage && c.lastMessage.seq <= clearedSeq ? null : c.lastMessage
  return { ...c, readSeq, clearedSeq, lastMessage, unread: c.lastSeq - readSeq }
}

function put(list: ChatSummary[]) {
  if (list.length === 0) return
  useChatListStore.setState((s) => {
    const chats = { ...s.chats }
    for (const c of list) chats[c.peer.id] = merge(chats[c.peer.id], c)
    return { chats }
  })
  const chats = useChatListStore.getState().chats
  void saveChats(list.map((c) => ({ ...chats[c.peer.id], peerId: c.peer.id })))
}

// 同一时刻只跑一个 load；期间又有新的 load 请求（例如推送来了一个列表里没有的会话），结束后再跑一次
let loading: Promise<void> | null = null
let reloadAfter = false

export const useChatListStore = create<ChatListState>()(() => ({
  chats: {},
  loaded: false,
  error: null,
  peers: {},

  rememberPeer: (p) => useChatListStore.setState((s) => ({ peers: { ...s.peers, [p.id]: p } })),

  load: () => {
    if (loading) {
      reloadAfter = true
      return loading
    }
    loading = (async () => {
      const token = currentToken()
      try {
        const { items } = await listChats()
        if (currentToken() !== token) return
        put(items)
        useChatListStore.setState({ loaded: true, error: null })
      } catch (err) {
        if (currentToken() === token) useChatListStore.setState({ error: errorMessage(err) })
      } finally {
        loading = null
        if (reloadAfter && currentToken() === token) {
          reloadAfter = false
          void useChatListStore.getState().load()
        }
      }
    })()
    return loading
  },

  loadSnapshot: async () => {
    const token = currentToken()
    const list = await loadChatSnapshot()
    if (currentToken() !== token) return
    useChatListStore.setState((s) => {
      const chats = { ...s.chats }
      for (const { peerId, ...c } of list) chats[peerId] = merge(chats[peerId], c)
      return { chats }
    })
  },
}))

// applyMessageToList 收到一条已确认的消息（推送、ACK、增量同步），更新对应会话的最后一条消息和已读位置
export function applyMessageToList(m: Message) {
  const me = myId()
  const mine = m.senderId === me
  const peerId = mine ? m.recipientId : m.senderId
  const cur = useChatListStore.getState().chats[peerId]

  if (!cur) {
    // 列表里还没有这个会话（第一次聊天，或者列表还没拉下来）：先用手头的信息建一项，再拉一次列表补全资料和准确的已读位置。
    // 对方消息的 readSeq 先按 0 算：列表拉回来后取较大值，以服务端为准
    const f = useFriendStore.getState().friends.find((x) => x.id === peerId)
    const hint = useChatListStore.getState().peers[peerId]
    put([
      {
        peer: f ? { id: f.id, username: f.username, avatarUrl: f.avatarUrl } : (hint ?? { id: peerId, username: '', avatarUrl: '' }),
        lastMessage: m,
        lastSeq: m.seq,
        readSeq: mine ? m.seq : 0,
        unread: 0,
        lastMessageAt: m.createdAt,
      },
    ])
    void useChatListStore.getState().load()
    return
  }

  const newer = m.seq > cur.lastSeq
  const advancesRead = mine && m.seq > cur.readSeq
  if (!newer && !advancesRead) return // 重复收到（ACK 和 message.new 各一次）或者是更早的消息
  put([
    {
      ...cur,
      ...(newer ? { lastMessage: m, lastSeq: m.seq, lastMessageAt: m.createdAt } : {}),
      readSeq: mine ? Math.max(cur.readSeq, m.seq) : cur.readSeq, // 发送即已读（FR-15）
    },
  ])
}

// applyReadToList 已读位置推进（本地乐观更新、上报接口的返回、其他标签页的 chat.read 推送）
export function applyReadToList(peerId: number, readSeq: number) {
  const cur = useChatListStore.getState().chats[peerId]
  if (!cur || readSeq <= cur.readSeq) return
  put([{ ...cur, readSeq }])
}

// applyDeletionToList 我删除了若干条消息（FR-19）：摘要正好是其中一条时，先清掉摘要，再从服务端拉列表取"能看到的最后一条"
export function applyDeletionToList(peerId: number, seqs: number[]) {
  const cur = useChatListStore.getState().chats[peerId]
  if (!cur?.lastMessage || !seqs.includes(cur.lastMessage.seq)) return
  put([{ ...cur, lastMessage: null }])
  void useChatListStore.getState().load()
}

// applyClearToList 清空聊天记录推进了清空位置和已读位置（FR-19：清空视为已读）
export function applyClearToList(peerId: number, clearedSeq: number, readSeq: number) {
  const cur = useChatListStore.getState().chats[peerId]
  if (!cur) return
  put([{ ...cur, clearedSeq: Math.max(cur.clearedSeq ?? 0, clearedSeq), readSeq: Math.max(cur.readSeq, readSeq) }])
}

// 自己在其他标签页 / 设备上读了消息（FR-04、FR-15）
socket.on('chat.read', (data) => {
  const d = data as { peerId: number; readSeq: number }
  applyReadToList(d.peerId, d.readSeq)
})

// 退出登录或切换账号时清空
useAuthStore.subscribe((state, prev) => {
  if (state.token === prev.token) return
  reloadAfter = false
  useChatListStore.setState({ chats: {}, loaded: false, error: null, peers: {} })
})
