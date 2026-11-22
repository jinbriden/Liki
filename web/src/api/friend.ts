import { http } from './client'

// 以下类型对应后端 internal/friend/handler.go 的 resp 结构体

// 搜索结果中对方与我的关系
export type Relation = 'none' | 'friend' | 'pending_out' | 'pending_in'

export type RequestStatus = 'pending' | 'accepted' | 'rejected'

// profileResp
export interface Profile {
  id: number
  username: string
  avatarUrl: string // 暂无上传功能，通常为空字符串
}

// searchItemResp：Go 里嵌入 profileResp，JSON 被"拍平"，对应 TS 的 extends
export interface SearchItem extends Profile {
  relation: Relation
}

// friendResp
export interface Friend extends Profile {
  since: string // 成为好友的时间（UTC）
}

// friendRequestResp。user 是"对方"：收到的申请里是申请人，发出的申请里是被申请人
export interface FriendRequest {
  id: number
  user: Profile
  message: string
  status: RequestStatus
  createdAt: string
  handledAt?: string
}

// sendRequestResp
export interface SentRequest {
  id: number
  toUserId: number
  message: string
  status: RequestStatus
  createdAt: string
}

// 后端列表统一包一层 {items: [...]}
interface List<T> {
  items: T[]
}

// 验证信息上限，与后端 maxMessageLen 一致
export const MAX_REQUEST_MESSAGE_LEN = 100

// 按用户名（不区分大小写，完全匹配）或 10 位用户 ID 搜索，结果不含自己
export async function searchUsers(q: string) {
  const res = await http.get<List<SearchItem>>(`/friends/search?q=${encodeURIComponent(q)}`)
  return res.items
}

export async function listFriends() {
  return (await http.get<List<Friend>>('/friends')).items
}

// 双向解除好友关系；对方本来就不是好友也返回成功（幂等）
export function deleteFriend(friendId: number) {
  return http.delete<null>(`/friends/${friendId}`)
}

// 同方向已有待处理申请时，后端幂等返回已有的那条
export function sendFriendRequest(toUserId: number, message: string) {
  return http.post<SentRequest>('/friend-requests', { toUserId, message })
}

// 我收到的待处理申请
export async function listIncomingRequests() {
  return (await http.get<List<FriendRequest>>('/friend-requests/incoming')).items
}

// 我发出的申请（含已处理），最多 50 条
export async function listOutgoingRequests() {
  return (await http.get<List<FriendRequest>>('/friend-requests/outgoing')).items
}

export function acceptRequest(id: number) {
  return http.post<null>(`/friend-requests/${id}/accept`)
}

export function rejectRequest(id: number) {
  return http.post<null>(`/friend-requests/${id}/reject`)
}
