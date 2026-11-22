import { issueWsTicket } from '../api/ws'
import { useAuthStore } from '../stores/auth'

// WebSocket 客户端：建连（先换票据）、心跳、断线自动重连、按 type 分发数据包。
// 业务代码只用 socket.on(type, fn) 订阅、socket.send(type, data) 发送，不直接碰 WebSocket 对象。
//
// 数据包格式和后端 internal/gateway/protocol.go 一致：{ type, version, data }

const PROTOCOL_VERSION = 1
const PING_INTERVAL = 25_000 // 每 25 秒发一次 ping；后端 60 秒收不到任何数据就断开
const PONG_TIMEOUT = 10_000 // 发出 ping 后 10 秒没收到 pong，认为连接已死（例如断网后 TCP 连接还没报错）
const MAX_BACKOFF = 30_000

export type SocketStatus = 'idle' | 'connecting' | 'open' | 'reconnecting'

interface Packet {
  type: string
  version: number
  data?: unknown
}

type Listener = (data: unknown) => void

// OpenInfo 连接建立时传给 onOpen 订阅者
export interface OpenInfo {
  resumed: boolean // 是否是断线后的重连（不是本次登录的第一次连接）
}

class RealtimeSocket {
  private ws: WebSocket | null = null
  private status: SocketStatus = 'idle'
  private wanted = false // 是否应该保持连接（已登录且在主界面）
  // 每次 start / stop / 重新建连都加 1。异步回调（换票据、onclose）回来时如果代数变了，说明已经过时，直接丢弃
  private generation = 0
  private retries = 0
  private everOpened = false
  private reconnectTimer: ReturnType<typeof setTimeout> | undefined
  private pingTimer: ReturnType<typeof setInterval> | undefined
  private pongTimer: ReturnType<typeof setTimeout> | undefined

  private listeners = new Map<string, Set<Listener>>()
  private openListeners = new Set<(info: OpenInfo) => void>()
  private statusListeners = new Set<(s: SocketStatus) => void>()

  constructor() {
    // 网络恢复时立刻重连，不用等退避计时器；断网时主动断开，界面能马上显示"已断开"
    window.addEventListener('online', () => {
      if (this.wanted && this.status !== 'open') {
        this.retries = 0
        this.connectNow()
      }
    })
    window.addEventListener('offline', () => {
      if (this.wanted && this.ws) this.drop()
    })
  }

  getStatus() {
    return this.status
  }

  // start 开始保持连接。重复调用无副作用
  start() {
    if (this.wanted) return
    this.wanted = true
    this.everOpened = false
    this.retries = 0
    this.connectNow()
  }

  // stop 断开并停止重连（退出登录、切换账号时）
  stop() {
    this.wanted = false
    this.generation++
    clearTimeout(this.reconnectTimer)
    this.stopHeartbeat()
    const ws = this.ws
    this.ws = null
    if (ws) {
      ws.onclose = null
      ws.close(1000)
    }
    this.setStatus('idle')
  }

  // send 发送数据包。连接没建立时返回 false，由调用方决定是否稍后重发
  send(type: string, data?: unknown): boolean {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return false
    this.ws.send(JSON.stringify({ type, version: PROTOCOL_VERSION, data } satisfies Packet))
    return true
  }

  // on 订阅某种数据包，返回取消订阅的函数
  on(type: string, fn: Listener): () => void {
    let set = this.listeners.get(type)
    if (!set) {
      set = new Set()
      this.listeners.set(type, set)
    }
    set.add(fn)
    return () => set.delete(fn)
  }

  // onOpen 每次连接建立（包括重连）时调用。断线期间可能漏掉推送，订阅者应在这里补齐数据
  onOpen(fn: (info: OpenInfo) => void): () => void {
    this.openListeners.add(fn)
    return () => this.openListeners.delete(fn)
  }

  onStatus(fn: (s: SocketStatus) => void): () => void {
    this.statusListeners.add(fn)
    return () => this.statusListeners.delete(fn)
  }

  private setStatus(s: SocketStatus) {
    if (this.status === s) return
    this.status = s
    this.statusListeners.forEach((fn) => fn(s))
  }

  private connectNow() {
    clearTimeout(this.reconnectTimer)
    void this.connect()
  }

  private async connect() {
    // 网络恢复事件可能在上一次建连还没完成时触发，先关掉旧的，保证同一时刻只有一个连接
    if (this.ws) {
      this.ws.onclose = null
      this.ws.close()
      this.ws = null
    }
    const gen = ++this.generation
    this.setStatus(this.everOpened ? 'reconnecting' : 'connecting')

    let ticket: string
    try {
      ticket = (await issueWsTicket()).ticket
    } catch {
      // 令牌失效（10003）时 api 层已经清除登录状态，主界面卸载时会调用 stop；其他错误（网络、服务端故障）稍后重试
      if (gen === this.generation && this.wanted) this.scheduleReconnect()
      return
    }
    if (gen !== this.generation || !this.wanted) return

    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    const ws = new WebSocket(`${proto}://${location.host}/api/v1/ws?ticket=${encodeURIComponent(ticket)}`)
    this.ws = ws

    ws.onopen = () => {
      if (gen !== this.generation) {
        ws.close() // 已过时的连接（期间调用过 stop 或重新建连），不能留着占用服务端资源
        return
      }
      const resumed = this.everOpened
      this.everOpened = true
      this.retries = 0
      this.setStatus('open')
      this.startHeartbeat()
      this.openListeners.forEach((fn) => fn({ resumed }))
    }

    ws.onmessage = (e) => {
      if (gen !== this.generation) return
      let p: Packet
      try {
        p = JSON.parse(String(e.data)) as Packet
      } catch {
        return
      }
      if (p.type === 'pong') {
        clearTimeout(this.pongTimer)
        return
      }
      if (p.type === 'error') {
        console.warn('[ws] server error packet', p.data)
      }
      this.listeners.get(p.type)?.forEach((fn) => fn(p.data))
    }

    // 握手失败、服务端关闭、网络断开都会走到 onclose（onerror 之后一定跟着 onclose，所以不单独处理 onerror）
    ws.onclose = () => {
      if (gen !== this.generation) return
      this.ws = null
      this.stopHeartbeat()
      if (this.wanted) this.scheduleReconnect()
    }
  }

  // drop 立刻丢弃当前连接并安排重连。用于心跳超时、断网：
  // 这时 TCP 连接可能已经失效，ws.close() 要等很久才触发 onclose，不能等它
  private drop() {
    const ws = this.ws
    this.ws = null
    this.generation++ // 让旧连接迟到的回调失效
    this.stopHeartbeat()
    if (ws) {
      ws.onclose = null
      ws.close()
    }
    if (this.wanted) this.scheduleReconnect()
  }

  // 指数退避：1s、2s、4s…最多 30s，加一点随机抖动，避免服务端重启后所有客户端同一时刻涌进来
  private scheduleReconnect() {
    this.setStatus('reconnecting')
    clearTimeout(this.reconnectTimer)
    const delay = Math.min(MAX_BACKOFF, 1000 * 2 ** this.retries) + Math.random() * 500
    this.retries++
    this.reconnectTimer = setTimeout(() => this.connectNow(), delay)
  }

  private startHeartbeat() {
    this.stopHeartbeat()
    this.pingTimer = setInterval(() => {
      if (!this.send('ping')) return
      clearTimeout(this.pongTimer)
      this.pongTimer = setTimeout(() => {
        console.warn('[ws] heartbeat timeout, reconnecting')
        this.drop()
      }, PONG_TIMEOUT)
    }, PING_INTERVAL)
  }

  private stopHeartbeat() {
    clearInterval(this.pingTimer)
    clearTimeout(this.pongTimer)
  }
}

// 全局只有一个连接，所有模块共用
export const socket = new RealtimeSocket()

// 令牌变化（退出登录、其他标签页切换账号）时立刻断开，不等页面卸载。
// 重新连接由主界面（MainLayout）在拿到新用户信息后调用 socket.start()
useAuthStore.subscribe((state, prev) => {
  if (state.token !== prev.token) socket.stop()
})
