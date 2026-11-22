import { http } from './client'

// 对应后端 user.userResp
export interface User {
  id: number // 10 位数，小于 2^53，JS number 可以精确表示
  username: string
  createdAt: string // UTC 时间，形如 2026-11-21T13:33:39.400657Z
}

// 对应后端 user.loginResp
export interface LoginResult {
  token: string
  expiresAt: string
  user: User
}

export function register(username: string, password: string) {
  return http.post<User>('/auth/register', { username, password })
}

export function login(userId: number, password: string) {
  return http.post<LoginResult>('/auth/login', { userId, password })
}

export function getMe() {
  return http.get<User>('/me')
}
