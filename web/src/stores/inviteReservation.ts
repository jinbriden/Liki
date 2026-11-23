import { create } from 'zustand'
import { Code, errorMessage, isApiError } from '../api/client'
import { releaseInvite, releaseInviteOnUnload, renewInvite, verifyInvite } from '../api/invite'

// 注册前的邀请码验证与冻结（FR-23）。
//
// 验证通过后邀请码被冻结，只有这个页面能用它提交注册申请。冻结是服务端的一份租约（3 分钟），
// 这里负责在注册页开着时每 30 秒续期、离开时解冻：
//   - 在站内离开注册页（点"登录"、浏览器后退）：立即解冻
//   - 刷新、关闭标签页（pagehide）：两者分不清，软释放（服务端只保留 15 秒）。
//     刷新的话新页面从 sessionStorage 读回令牌马上续期，冻结恢复，不算重新验证；关闭的话 15 秒后码恢复可用
//   - 浏览器崩溃、断网：不再续期，最多 3 分钟后服务端自动解冻
// 令牌只放 sessionStorage（只在当前标签页、关闭即清除），不放 localStorage。

const STORAGE_KEY = 'liki.inviteReservation'
const RENEW_INTERVAL_MS = 30_000
const MSG_LOST = '邀请码的验证已失效，请重新验证'

interface Held {
  code: string
  token: string
  expiresAt: string // 邀请码本身的过期时间，显示给用户
}

function load(): Held | null {
  try {
    const raw = sessionStorage.getItem(STORAGE_KEY)
    return raw ? (JSON.parse(raw) as Held) : null
  } catch {
    return null
  }
}

function save(held: Held | null) {
  try {
    if (held) sessionStorage.setItem(STORAGE_KEY, JSON.stringify(held))
    else sessionStorage.removeItem(STORAGE_KEY)
  } catch {
    // 存不进去只影响"刷新后不用重新验证"，不影响当前页面
  }
}

// idle：显示邀请码输入框；verifying / resuming：请求中；held：已冻结，显示注册表单
type Phase = 'idle' | 'verifying' | 'resuming' | 'held'

interface ReservationState {
  phase: Phase
  held: Held | null
  lastCode: string // 最近一次输入 / 验证的码，回到输入框时预填
  error: string | null

  verify: (code: string) => Promise<void>
  resume: () => Promise<void>
  release: () => void
  markUsed: () => void
  lose: (msg?: string) => void
  clearError: () => void
}

// 续期定时器和"注册页是否在显示"放在模块里，不放进 state：它们不影响渲染
let renewTimer: ReturnType<typeof setInterval> | undefined
let attached = false
let pendingRelease: ReturnType<typeof setTimeout> | undefined

function stopRenewing() {
  clearInterval(renewTimer)
  renewTimer = undefined
}

function startRenewing() {
  stopRenewing()
  if (attached && useInviteReservation.getState().held) {
    renewTimer = setInterval(() => void renewNow(), RENEW_INTERVAL_MS)
  }
}

async function renewNow() {
  const held = useInviteReservation.getState().held
  if (!held) return
  try {
    await renewInvite(held.code, held.token)
  } catch (err) {
    // 网络抖动等：下一轮再试（租约 3 分钟，够重试几次）；冻结确实失效了才回到输入邀请码
    if (isApiError(err, Code.ReservationLost) && useInviteReservation.getState().held === held) {
      useInviteReservation.getState().lose()
    }
  }
}

export const useInviteReservation = create<ReservationState>()((set, get) => ({
  phase: 'idle',
  held: null,
  lastCode: '',
  error: null,

  verify: async (code) => {
    const { phase, held, lastCode } = get()
    // 开发模式（StrictMode）下 effect 会执行两次，同一个码正在验证或已经冻结时不重复请求：
    // 重复验证会被服务端当成"使用中"或"1 分钟内只能验证一次"
    if ((phase === 'verifying' && lastCode === code) || held?.code === code) return
    if (held) get().release() // 换了一个码：先解冻原来的

    set({ phase: 'verifying', lastCode: code, error: null })
    try {
      const r = await verifyInvite(code)
      const next: Held = { code, token: r.reservationToken, expiresAt: r.expiresAt }
      save(next)
      set({ phase: 'held', held: next })
      startRenewing()
    } catch (err) {
      set({ phase: 'idle', error: errorMessage(err) })
    }
  },

  // resume 刷新页面后，从 sessionStorage 读回令牌续期，不算重新验证
  resume: async () => {
    const stored = load()
    if (!stored || get().phase !== 'idle') return
    set({ phase: 'resuming', lastCode: stored.code, error: null })
    try {
      await renewInvite(stored.code, stored.token)
      set({ phase: 'held', held: stored })
      startRenewing()
    } catch (err) {
      if (isApiError(err, Code.ReservationLost)) {
        save(null)
        set({ phase: 'idle', error: MSG_LOST })
      } else {
        // 网络问题：令牌可能仍然有效，先按已冻结显示，续期定时器会继续确认
        set({ phase: 'held', held: stored })
        startRenewing()
      }
    }
  },

  // release 主动离开（更换邀请码、离开注册页）：立即解冻，码恢复可用
  release: () => {
    const { held } = get()
    stopRenewing()
    save(null)
    set({ phase: 'idle', held: null })
    if (held) releaseInvite(held.code, held.token).catch(() => {}) // 失败也没关系，3 分钟后服务端自动解冻
  },

  // markUsed 注册申请提交成功：邀请码已经失效，不需要解冻
  markUsed: () => {
    stopRenewing()
    save(null)
    set({ phase: 'idle', held: null, lastCode: '', error: null })
  },

  // lose 冻结已失效（到期、被作废）：回到输入邀请码，预填原来的码
  lose: (msg = MSG_LOST) => {
    stopRenewing()
    save(null)
    set((s) => ({ phase: 'idle', held: null, lastCode: s.held?.code ?? s.lastCode, error: msg }))
  },

  clearError: () => set({ error: null }),
}))

// attachRegisterPage 注册页挂载时调用，返回的函数在卸载时调用（作为 useEffect 的清理函数）。
// 卸载时不马上解冻，而是推迟到下一个宏任务：开发模式（StrictMode）会"挂载 → 卸载 → 再挂载"一次，
// 再挂载时取消这次解冻；真正离开注册页才会执行
export function attachRegisterPage(): () => void {
  attached = true
  clearTimeout(pendingRelease)
  startRenewing()
  return () => {
    attached = false
    stopRenewing()
    pendingRelease = setTimeout(() => {
      if (!attached) useInviteReservation.getState().release()
    }, 0)
  }
}

// 页面卸载（刷新、关闭标签页）：软释放。sessionStorage 里的令牌保留，刷新后 resume 续期即可恢复
window.addEventListener('pagehide', () => {
  const { held } = useInviteReservation.getState()
  if (held && attached) releaseInviteOnUnload(held.code, held.token)
})

// 标签页切回前台时马上续期一次：后台标签页的定时器会被浏览器节流，可能已经很久没续期了
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible' && attached && useInviteReservation.getState().held) {
    void renewNow()
  }
})

// 从往返缓存（bfcache）恢复页面（离开后按"后退"回来）：离开时已经软释放，马上续期把冻结要回来
window.addEventListener('pageshow', (e) => {
  if (e.persisted && attached && useInviteReservation.getState().held) void renewNow()
})
