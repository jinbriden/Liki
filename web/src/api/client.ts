import { useAuthStore } from '../stores/auth'

// 与后端 internal/pkg/response 中的错误码保持一致
export const Code = {
  OK: 0,
  InvalidParam: 10001, // 参数不符合业务规则，msg 可以直接展示给用户
  BadRequest: 10002, // 请求体无法解析，属于前端代码 bug
  Unauthorized: 10003, // 令牌缺失/无效/过期：清除令牌并跳转登录页
  InvalidCredentials: 20001, // 登录失败：同为 HTTP 401，但不跳转
  // 好友模块
  CannotAddSelf: 30001,
  AlreadyFriends: 30002,
  ReverseRequestPending: 30003, // 对方已向你发送申请，应去申请列表处理
  RequestNotFound: 30004,
  RequestHandled: 30005, // 申请已被处理（比如在另一个标签页处理过）
  TargetUserNotFound: 30006,
  // 消息模块
  MessageEmpty: 40001,
  MessageTooLong: 40002, // 超过 1500 字：消息标记为发送失败，只保留在本地
  NotFriends: 40003, // 对方不是好友，不能发消息
  Internal: 50000,
  // 以下为前端自定义，不会出现在后端响应里
  Network: -1, // 请求没发出去或没收到响应
  BadResponse: -2, // 响应不是约定的 {code, msg, data} 格式
  ServerUnavailable: -3, // 5xx 且不是后端的 JSON：后端没启动、崩溃或维护中，由代理（Vite / Nginx）返回
} as const

const GENERIC_MSG = '请求失败，请稍后重试'

// ApiError 统一的接口错误。页面根据 code 决定如何展示。
export class ApiError extends Error {
  readonly code: number

  constructor(code: number, msg: string) {
    super(msg)
    this.name = 'ApiError'
    this.code = code
  }
}

// errorMessage 把任意错误转成可展示给用户的文字。
// 10002 说明前端发的请求格式有 bug，后端的提示对用户没有意义，统一换成通用提示。
export function errorMessage(err: unknown): string {
  if (err instanceof ApiError && err.code !== Code.BadRequest) {
    return err.message
  }
  return GENERIC_MSG
}

interface Body<T> {
  code: number
  msg: string
  data?: T
}

// request 发送 JSON 请求，成功时返回 data，失败时抛出 ApiError。
// 判断成功与否看业务 code，不看 HTTP 状态码：同样是 401，10003 和 20001 的处理方式不同。
async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {}
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json'
  }
  const token = useAuthStore.getState().token
  if (token) {
    headers['Authorization'] = `Bearer ${token}`
  }

  let res: Response
  try {
    res = await fetch(`/api/v1${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    })
  } catch {
    throw new ApiError(Code.Network, '网络异常，请检查网络后重试')
  }

  let json: Body<T>
  try {
    json = (await res.json()) as Body<T>
  } catch {
    // 后端的响应一定是 JSON（包括 50000 这类内部错误）。5xx 又不是 JSON，
    // 说明请求没到达后端，是中间的代理返回的，例如后端未启动时 Vite 代理返回空响应体的 502。
    if (res.status >= 500) {
      throw new ApiError(Code.ServerUnavailable, '服务器出现故障或正在维护，请稍后再试')
    }
    throw new ApiError(Code.BadResponse, GENERIC_MSG)
  }
  if (typeof json?.code !== 'number') {
    throw new ApiError(Code.BadResponse, GENERIC_MSG)
  }

  if (json.code !== Code.OK) {
    if (json.code === Code.Unauthorized) {
      // 只清除登录状态，不在这里跳转：路由守卫看到令牌没了，会自动重定向到登录页。
      // 这样 api 层不依赖路由，和后端 repository 不依赖 gin 是一个道理。
      useAuthStore.getState().clear()
    }
    throw new ApiError(json.code, json.msg || GENERIC_MSG)
  }
  return json.data as T
}

// isApiError 判断错误是否为指定业务码，用于页面针对个别错误做特殊处理
export function isApiError(err: unknown, ...codes: number[]): err is ApiError {
  return err instanceof ApiError && codes.includes(err.code)
}

export const http = {
  get: <T>(path: string) => request<T>('GET', path),
  post: <T>(path: string, body?: unknown) => request<T>('POST', path, body),
  delete: <T>(path: string) => request<T>('DELETE', path),
}
