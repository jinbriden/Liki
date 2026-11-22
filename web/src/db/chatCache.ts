import { Dexie, type Table } from 'dexie'
import type { ChatSummary, Message } from '../api/message'
import type { ChatMessage } from '../stores/chatTypes'
import { addRange, type Range } from '../utils/ranges'

// 聊天的本地缓存（FR-16），浏览器 IndexedDB，用 Dexie 封装。
//
// 每个用户一个独立的库（liki-<用户 ID>），同一浏览器里切换账号互不影响；主动退出登录时整库删除（FR-03）。
// 同一用户的多个标签页共用一个库：写入都按主键覆盖（幂等），区间合并放在事务里，不会互相踩坏。
//
// 缓存只是加速和离线展示，服务端才是准的：这里所有读写失败（隐私模式禁用存储、配额满、库被其他标签页删掉等）
// 都只打一条警告，读返回空、写直接忽略，聊天功能退化为只用内存，照样能用。

// 已确认的消息：在 Message 上加 peerId（会话按对方用户 ID 组织），主键 [peerId+seq]
export interface StoredMessage extends Message {
  peerId: number
}

// 本地还没被服务端确认的消息（发送中、发送失败、超长），主键 key
export interface StoredLocal extends ChatMessage {
  peerId: number
}

interface StoredRanges {
  peerId: number
  ranges: Range[]
}

// 会话列表快照，打开页面时先显示它，再用服务端结果刷新
export interface StoredChat extends ChatSummary {
  peerId: number
}

class ChatDB extends Dexie {
  messages!: Table<StoredMessage, [number, number]>
  outbox!: Table<StoredLocal, string>
  ranges!: Table<StoredRanges, number>
  chats!: Table<StoredChat, number>

  constructor(name: string) {
    super(name)
    // 只列主键和需要查询的索引，其余字段 Dexie 照样会存。以后改结构时加 version(2)，类似数据库迁移
    this.version(1).stores({
      messages: '[peerId+seq]', // 复合主键：按会话 + seq 范围查询（翻页、跳转）
      outbox: 'key, peerId',
      ranges: 'peerId',
      chats: 'peerId',
    })
  }
}

const dbName = (userId: number) => `liki-${userId}`

let db: ChatDB | null = null

// openCache 登录后打开当前用户的库；已经打开同一个库时什么也不做
export function openCache(userId: number) {
  if (db?.name === dbName(userId)) return
  db?.close()
  db = new ChatDB(dbName(userId))
}

// closeCache 退出登录、切换账号时关闭（不删除）
export function closeCache() {
  db?.close()
  db = null
}

// deleteCache 主动退出登录时删除该用户的整个库（FR-03）。
// 其他标签页如果还开着这个库，Dexie 会收到 versionchange 事件并自动关闭，删除不会被一直挡住
export async function deleteCache(userId: number) {
  closeCache()
  try {
    await Dexie.delete(dbName(userId))
  } catch (err) {
    console.warn('[cache] delete failed', err)
  }
}

// safe 执行一次缓存操作：库没打开或出错时返回 fallback。
// 调用时把 db 取出来传进去，异步过程中切换了账号也只会操作旧库（已关闭，报错被吞掉），不会写进新账号的库
async function safe<T>(fallback: T, fn: (d: ChatDB) => Promise<T>): Promise<T> {
  const d = db
  if (!d) return fallback
  try {
    return await fn(d)
  } catch (err) {
    console.warn('[cache] operation failed', err)
    return fallback
  }
}

// ---------- 消息与已同步区间 ----------

// saveMessages 写入一批已确认的消息，并把 [lo, hi] 记为已同步区间（不传 range 则只写消息）。返回合并后的区间
export function saveMessages(peerId: number, msgs: Message[], range?: Range): Promise<Range[] | null> {
  return safe(null, (d) =>
    d.transaction('rw', d.messages, d.ranges, d.outbox, async () => {
      if (msgs.length > 0) {
        await d.messages.bulkPut(msgs.map((m) => ({ ...m, peerId })))
        // 自己发的消息被确认了，从待发送里删掉
        await d.outbox.bulkDelete(msgs.map((m) => `${m.senderId}:${m.clientMsgId}`))
      }
      const cur = (await d.ranges.get(peerId))?.ranges ?? []
      if (!range) return cur
      const next = addRange(cur, range[0], range[1])
      await d.ranges.put({ peerId, ranges: next })
      return next
    }),
  )
}

export function loadRanges(peerId: number): Promise<Range[]> {
  return safe([] as Range[], async (d) => (await d.ranges.get(peerId))?.ranges ?? [])
}

// readMessages 读 seq 在 [lo, hi] 内的消息，按 seq 升序返回。
// from = 'end' 时取最靠后的 limit 条（向上翻页、显示最新一页），'start' 时取最靠前的 limit 条（向下加载、跳转）
export function readMessages(peerId: number, lo: number, hi: number, limit: number, from: 'start' | 'end') {
  return safe([] as StoredMessage[], async (d) => {
    if (lo > hi) return []
    let q = d.messages.where('[peerId+seq]').between([peerId, lo], [peerId, hi], true, true)
    if (from === 'end') q = q.reverse()
    const list = await q.limit(limit).toArray()
    return from === 'end' ? list.reverse() : list
  })
}

// ---------- 待发送 ----------

export function putLocal(peerId: number, m: ChatMessage) {
  return safe(undefined, async (d) => {
    await d.outbox.put({ ...m, peerId })
  })
}

export function deleteLocal(key: string) {
  return safe(undefined, (d) => d.outbox.delete(key))
}

export function loadAllLocal() {
  return safe([] as StoredLocal[], (d) => d.outbox.toArray())
}

// ---------- 会话列表快照 ----------

export function loadChatSnapshot() {
  return safe([] as StoredChat[], (d) => d.chats.toArray())
}

// saveChats 写入（覆盖）若干会话。会话不会消失（第一阶段不能删除或隐藏会话），所以只写不删
export function saveChats(list: StoredChat[]) {
  return safe(undefined, async (d) => {
    await d.chats.bulkPut(list)
  })
}
