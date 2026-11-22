import { deleteCache } from '../db/chatCache'
import { useAuthStore } from './auth'

// logout 主动退出登录（FR-03）：清除令牌（WebSocket 随之断开，各 store 随之清空），再删除该用户的本地缓存。
// 令牌过期导致的被动登出只调用 auth.clear()，不删缓存：同一个人重新登录后还能直接用。
export async function logout() {
  const userId = useAuthStore.getState().user?.id
  useAuthStore.getState().clear()
  if (userId) await deleteCache(userId)
}
