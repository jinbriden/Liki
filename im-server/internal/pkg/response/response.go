package response

import "github.com/gin-gonic/gin"

const (
	CodeOk           = 0
	CodeInvalidParam = 10001 // 参数不符合业务规则：用户输入不合规，msg 可直接展示给用户
	CodeBadRequest   = 10002 // 请求体无法解析（JSON 格式/类型错误），通常是调用方代码的 bug
	CodeUnauthorized = 10003 // 未登录或令牌无效/过期：前端收到后应清除令牌并跳转登录页

	CodeInvalidCredentials = 20001 // 登录失败：用户 ID 或密码错误（同为 401，但前端不应跳转登录页）

	// 好友模块 3xxxx
	CodeCannotAddSelf         = 30001 // 不能添加自己为好友
	CodeAlreadyFriends        = 30002 // 已经是好友
	CodeReverseRequestPending = 30003 // 对方已向你发送申请，应去申请列表处理
	CodeRequestNotFound       = 30004 // 申请不存在（或不是发给你的）
	CodeRequestHandled        = 30005 // 申请已被处理
	CodeTargetUserNotFound    = 30006 // 目标用户不存在

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
