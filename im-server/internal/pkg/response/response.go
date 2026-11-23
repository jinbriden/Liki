package response

import "github.com/gin-gonic/gin"

const (
	CodeOk              = 0
	CodeInvalidParam    = 10001 // 参数不符合业务规则：用户输入不合规，msg 可直接展示给用户
	CodeBadRequest      = 10002 // 请求体无法解析（JSON 格式/类型错误），通常是调用方代码的 bug
	CodeUnauthorized    = 10003 // 未登录或令牌无效/过期：前端收到后应清除令牌并跳转登录页
	CodeTooManyRequests = 10004 // 请求太频繁，被限流（NFR-05），HTTP 429：msg 可直接展示，稍后重试即可
	CodeForbidden       = 10005 // 已登录，但没有权限做这件事（如非管理员生成邀请码），HTTP 403

	CodeInvalidCredentials = 20001 // 登录失败：用户 ID 或密码错误（同为 401，但前端不应跳转登录页）
	CodeAccountPending     = 20002 // 登录失败：密码正确，账号待审核（FR-22），HTTP 403
	CodeAccountRejected    = 20003 // 登录失败：密码正确，注册未通过审核（FR-22），HTTP 403
	CodeInvalidInvite      = 20004 // 验证邀请码失败：不存在、过期、已使用或已作废（不区分，FR-23）
	CodeInviteNotFound     = 20005 // 邀请码不存在，或不是自己生成的（查看、作废时）
	CodeInviteInUse        = 20006 // 邀请码有效，但正被别人使用（对方验证通过后正在填写注册表单），HTTP 409
	CodeReservationLost    = 20007 // 邀请码的验证已失效（冻结到期、被作废），续期、提交注册时返回，前端回到验证那一步

	// 好友模块 3xxxx
	CodeCannotAddSelf         = 30001 // 不能添加自己为好友
	CodeAlreadyFriends        = 30002 // 已经是好友
	CodeReverseRequestPending = 30003 // 对方已向你发送申请，应去申请列表处理
	CodeRequestNotFound       = 30004 // 申请不存在（或不是发给你的）
	CodeRequestHandled        = 30005 // 申请已被处理
	CodeTargetUserNotFound    = 30006 // 目标用户不存在

	// 消息模块 4xxxx
	CodeMessageEmpty   = 40001 // 消息内容为空
	CodeMessageTooLong = 40002 // 消息超过 1500 字（FR-13）：前端把这条消息标记为发送失败，只保留在本地
	// 40003 已废弃：M4、M5 阶段表示"非好友一律不能发"，M6 按 FR-12 放宽后不再返回，编号不再复用
	CodeStrangerLimit = 40004 // 非好友之间，对方回复或成为好友前只能发一条消息（FR-12）

	CodeInternal = 50000
)

type Body struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data,omitempty"`
}

func OK(c *gin.Context, data any) {
	c.JSON(200, Body{
		Code: CodeOk,
		Msg:  "ok",
		Data: data,
	})
}

func Fail(c *gin.Context, httpStatus, code int, msg string) {
	c.JSON(httpStatus, Body{Code: code, Msg: msg})
}
