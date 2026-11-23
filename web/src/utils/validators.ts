// 与后端 internal/user/service.go 中的校验规则保持一致。
// 前端校验只为即时提示，最终以后端校验为准。

export const MSG_INVALID_USERNAME = '用户名需为 3-32 位英文字母或数字'
export const MSG_INVALID_PASSWORD =
  '密码需为 8-24 位，至少包含一个大写字母、一个小写字母和一个数字，只能使用英文字母、数字和英文符号'
export const MSG_INVALID_USER_ID = '用户 ID 为 10 位数字'

// 用户名：3-32 位英文字母或数字
const USERNAME_RE = /^[A-Za-z0-9]{3,32}$/

// 密码：8-24 位可打印 ASCII（0x21-0x7E，不含空格），且同时包含大写、小写、数字
const PASSWORD_CHARSET_RE = /^[\x21-\x7E]{8,24}$/

// 用户 ID：后端范围 [1000000000, 9999999999]，即首位非 0 的 10 位数
const USER_ID_RE = /^[1-9]\d{9}$/

export function isValidUsername(s: string): boolean {
  return USERNAME_RE.test(s)
}

export function isValidPassword(s: string): boolean {
  return PASSWORD_CHARSET_RE.test(s) && /[A-Z]/.test(s) && /[a-z]/.test(s) && /\d/.test(s)
}

export function isValidUserId(s: string): boolean {
  return USER_ID_RE.test(s)
}

export const MSG_INVALID_INVITE_CODE = '邀请码为 6 位数字或英文字母'

// 邀请码：6 位数字 + 大写字母。输入不区分大小写，提交前转成大写（后端同样会转）
const INVITE_CODE_RE = /^[0-9A-Z]{6}$/

export function normalizeInviteCode(s: string): string {
  return s.trim().toUpperCase()
}

export function isValidInviteCode(s: string): boolean {
  return INVITE_CODE_RE.test(normalizeInviteCode(s))
}
