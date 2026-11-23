import type { AccountStatus } from './auth'
import { http } from './client'

// 以下类型对应后端 internal/user/invite_handler.go 的 resp 结构体（FR-23）

// 邀请码状态：可用 / 冻结中（有人验证通过，正在填写注册表单）/ 已使用 / 已过期 / 已作废
export type InviteState = 'active' | 'reserved' | 'used' | 'expired' | 'revoked'

// 一次验证的结果：验证成功并冻结 / 无效 / 正被别人使用 / 验证太频繁
export type VerifyResult = 'reserved' | 'invalid' | 'in_use' | 'limited'

// 用某个邀请码提交注册申请的账号（溯源）
export interface Invitee {
  id: number
  username: string
  status: AccountStatus
  createdAt: string
}

export interface Verification {
  ip: string
  result: VerifyResult
  createdAt: string
}

export interface Invite {
  id: number
  code: string // 6 位数字 + 大写字母
  note: string
  state: InviteState
  expiresAt: string
  usedAt: string | null
  revokedAt: string | null
  reservedUntil: string | null // 只在冻结中时有值
  createdAt: string
  usedBy: Invitee | null // 一个码只能提交一次注册申请
  verifications: Verification[] // 最新的在前
}

// 验证通过后的冻结。reservationToken 只在验证时返回，续期、提交注册都要带上它
export interface Reservation {
  reservationToken: string
  expiresAt: string // 邀请码本身的过期时间，冻结不会延长它
  reservedUntil: string
}

export const MAX_INVITE_NOTE_LEN = 50

// inviteLink 邀请链接。邀请码放在 # 后面：浏览器不会把 # 之后的部分发给服务器，不会出现在访问日志里
export function inviteLink(code: string): string {
  return `${location.origin}/register#invite=${encodeURIComponent(code)}`
}

// ---------- 注册页：验证、续期、解冻（无需登录） ----------

export function verifyInvite(inviteCode: string) {
  return http.post<Reservation>('/auth/invite/verify', { inviteCode })
}

export function renewInvite(inviteCode: string, reservationToken: string) {
  return http.post<Omit<Reservation, 'reservationToken'>>('/auth/invite/renew', { inviteCode, reservationToken })
}

export function releaseInvite(inviteCode: string, reservationToken: string) {
  return http.post<null>('/auth/invite/release', { inviteCode, reservationToken })
}

// releaseInviteOnUnload 页面卸载（刷新、关闭标签页）时解冻。用 sendBeacon：浏览器保证页面卸载后请求仍会发出，
// 普通 fetch 会被取消。分不清是刷新还是关闭，所以是软释放（服务端只保留 15 秒，刷新后的页面续期即可恢复）。
// Blob 用 text/plain：sendBeacon 只允许"简单请求"的类型，后端按 JSON 解析请求体，不看 Content-Type
export function releaseInviteOnUnload(inviteCode: string, reservationToken: string) {
  const body = new Blob([JSON.stringify({ inviteCode, reservationToken, soft: true })], { type: 'text/plain' })
  navigator.sendBeacon('/api/v1/auth/invite/release', body)
}

// ---------- 管理：生成、列表、作废（需要登录，现阶段只有管理员） ----------

// 网页生成固定用默认有效期（3 天），自定义只在服务器命令行里提供
export function createInvite(note: string) {
  return http.post<Invite>('/invites', { note })
}

export function listInvites() {
  return http.get<{ items: Invite[] }>('/invites').then((r) => r.items)
}

export function revokeInvite(id: number) {
  return http.post<Invite>(`/invites/${id}/revoke`)
}
