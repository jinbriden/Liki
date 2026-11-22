import { create } from 'zustand'
import {
  acceptRequest,
  deleteFriend,
  listFriends,
  listIncomingRequests,
  listOutgoingRequests,
  rejectRequest,
  sendFriendRequest,
  type Friend,
  type FriendRequest,
} from '../api/friend'
import { Code, errorMessage, isApiError } from '../api/client'
import { socket } from '../realtime/socket'
import { useAuthStore } from './auth'

// 好友模块的全局状态。页面只读这里的数据、调这里的方法，不直接调 api。
// WebSocket 推送（新申请、申请被处理、其他标签页的改动）也只更新这个 store，页面不用改。
interface FriendState {
  friends: Friend[]
  incoming: FriendRequest[] // 收到的待处理申请
  outgoing: FriendRequest[] // 发出的申请（含已处理）
  loading: boolean
  loaded: boolean // 好友列表至少成功加载过一次
  error: string | null

  loadAll: () => Promise<void>
  loadFriends: () => Promise<void>
  loadIncoming: () => Promise<void>
  loadOutgoing: () => Promise<void>
  accept: (requestId: number) => Promise<void>
  reject: (requestId: number) => Promise<void>
  send: (toUserId: number, message: string) => Promise<void>
  remove: (friendId: number) => Promise<void>
}

const initialData = {
  friends: [],
  incoming: [],
  outgoing: [],
  loading: false,
  loaded: false,
  error: null,
} satisfies Partial<FriendState>

// 请求发出时记下当前令牌；响应回来时如果令牌已经变了（退出登录、切换账号），
// 说明这是上一个账号的数据，丢弃，避免把 A 的好友列表显示给 B。
function sameSession<T>(fn: () => Promise<T>) {
  const token = useAuthStore.getState().token
  return async (apply: (v: T) => void) => {
    const v = await fn()
    if (useAuthStore.getState().token === token) apply(v)
  }
}

export const useFriendStore = create<FriendState>()((set, get) => ({
  ...initialData,

  loadAll: async () => {
    set({ loading: true, error: null })
    try {
      await Promise.all([get().loadFriends(), get().loadIncoming(), get().loadOutgoing()])
      set({ loading: false })
    } catch (err) {
      set({ loading: false, error: errorMessage(err) })
      throw err
    }
  },

  loadFriends: () => sameSession(listFriends)((friends) => set({ friends, loaded: true })),
  loadIncoming: () => sameSession(listIncomingRequests)((incoming) => set({ incoming })),
  loadOutgoing: () => sameSession(listOutgoingRequests)((outgoing) => set({ outgoing })),

  accept: async (requestId) => {
    try {
      await acceptRequest(requestId)
    } catch (err) {
      // 申请已不存在或已处理（比如在另一个标签页处理过）：刷新列表，让界面和服务端一致
      if (isApiError(err, Code.RequestNotFound, Code.RequestHandled)) void get().loadIncoming().catch(() => {})
      throw err
    }
    set({ incoming: get().incoming.filter((r) => r.id !== requestId) })
    await get().loadFriends()
  },

  reject: async (requestId) => {
    try {
      await rejectRequest(requestId)
    } catch (err) {
      if (isApiError(err, Code.RequestNotFound, Code.RequestHandled)) void get().loadIncoming().catch(() => {})
      throw err
    }
    set({ incoming: get().incoming.filter((r) => r.id !== requestId) })
  },

  send: async (toUserId, message) => {
    try {
      await sendFriendRequest(toUserId, message)
    } catch (err) {
      // 本地数据已经过时：对方先申请了我 / 我们已经是好友，刷新对应列表
      if (isApiError(err, Code.ReverseRequestPending)) void get().loadIncoming().catch(() => {})
      if (isApiError(err, Code.AlreadyFriends)) void get().loadFriends().catch(() => {})
      throw err
    }
    void get().loadOutgoing().catch(() => {})
  },

  remove: async (friendId) => {
    await deleteFriend(friendId)
    set({ friends: get().friends.filter((f) => f.id !== friendId) })
  },
}))

// 退出登录或切换账号时清空好友数据。类似 Spring 的事件监听：auth store 不需要知道好友模块的存在。
useAuthStore.subscribe((state, prev) => {
  if (state.token !== prev.token) {
    useFriendStore.setState(initialData)
  }
})

// ---------- 实时推送（FR-07、FR-04） ----------

const quiet = (p: Promise<void>) => void p.catch(() => {}) // 后台刷新失败不打扰用户，好友页会显示错误

// 有人向我发了好友申请：直接插到列表最前面，导航栏角标立刻更新
socket.on('friend.request.received', (data) => {
  const req = data as FriendRequest
  const { incoming } = useFriendStore.getState()
  if (!incoming.some((r) => r.id === req.id)) useFriendStore.setState({ incoming: [req, ...incoming] })
})

// 我发出的申请被处理了：刷新"我发出的申请"，同意时还要刷新好友列表
socket.on('friend.request.handled', (data) => {
  const req = data as FriendRequest
  quiet(useFriendStore.getState().loadOutgoing())
  if (req.status === 'accepted') quiet(useFriendStore.getState().loadFriends())
})

// 我在其他标签页 / 设备上改了好友数据（发申请、处理申请、删好友）
socket.on('friend.sync', () => quiet(useFriendStore.getState().loadAll()))

// 断线期间可能漏掉推送，重连后整体刷新一次
socket.onOpen(({ resumed }) => {
  if (resumed) quiet(useFriendStore.getState().loadAll())
})
