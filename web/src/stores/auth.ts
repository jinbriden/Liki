import { create } from 'zustand'
import type { User } from '../api/auth'

// 令牌只存在 localStorage 的这一个键里，读写都集中在本文件。
// 以后换存储方式（如 HttpOnly Cookie）只需要改这里。
const TOKEN_KEY = 'liki.token'

function readToken(): string | null {
  try {
    return localStorage.getItem(TOKEN_KEY)
  } catch {
    return null // 浏览器禁用存储时按未登录处理
  }
}

function writeToken(token: string | null) {
  try {
    if (token) {
      localStorage.setItem(TOKEN_KEY, token)
    } else {
      localStorage.removeItem(TOKEN_KEY)
    }
  } catch {
    // 写不进去也不影响当前标签页使用，只是刷新后需要重新登录
  }
}

interface AuthState {
  token: string | null
  // user 不持久化：刷新页面后由路由守卫调用 /me 重新获取，顺便验证令牌是否仍然有效
  user: User | null

  setAuth: (token: string, user: User) => void
  setUser: (user: User) => void
  clear: () => void
}

export const useAuthStore = create<AuthState>()((set) => ({
  token: readToken(),
  user: null,

  setAuth: (token, user) => {
    writeToken(token)
    set({ token, user })
  },
  setUser: (user) => set({ user }),
  // 令牌失效（10003）时调用；主动退出登录走 stores/session.ts 的 logout（它会先调用这里，再删除本地缓存）。
  // WebSocket 订阅了令牌变化，会随之断开（见 realtime/socket.ts）
  clear: () => {
    writeToken(null)
    set({ token: null, user: null })
  },
}))

// 多标签页同步：其他标签页登录或退出时，浏览器会在本标签页触发 storage 事件。
// 令牌变了就丢掉旧的 user，由路由守卫按新令牌重新获取（或跳转登录页）。
// key 为 null 表示整个 localStorage 被清空。
window.addEventListener('storage', (e) => {
  if (e.key !== TOKEN_KEY && e.key !== null) return
  const token = readToken()
  if (token !== useAuthStore.getState().token) {
    useAuthStore.setState({ token, user: null })
  }
})
