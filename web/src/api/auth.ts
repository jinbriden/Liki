import { http } from './client'

// 对应后端 user.userResp（登录、/me 返回的当前用户）
export interface User {
  id: number // 10 位数，小于 2^53，JS number 可以精确表示
  username: string
  createdAt: string // UTC 时间，形如 2026-11-21T13:33:39.400657Z
  canInvite: boolean // 能否生成邀请码（现阶段只有管理员），决定是否显示"邀请注册"入口
}

// 账号审核状态（FR-22），对应后端 user.AccountStatus
export type AccountStatus = 'pending' | 'active' | 'rejected'

// 对应后端 user.registerResp。注册成功后账号是待审核状态，不能直接登录
export interface RegisterResult {
  id: number
  username: string
  status: AccountStatus
  createdAt: string
}

// 对应后端 user.loginResp
export interface LoginResult {
  token: string
  expiresAt: string
  user: User
}

// register 提交注册申请。必须先验证邀请码（api/invite.ts 的 verifyInvite）拿到冻结令牌；
// 提交成功后邀请码立即失效（FR-23）
export function register(username: string, password: string, inviteCode: string, reservationToken: string) {
  return http.post<RegisterResult>('/auth/register', { username, password, inviteCode, reservationToken })
}

export function login(userId: number, password: string) {
  return http.post<LoginResult>('/auth/login', { userId, password })
}

export function getMe() {
  return http.get<User>('/me')
}
