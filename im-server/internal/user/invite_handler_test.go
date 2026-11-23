package user

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"im-server/internal/pkg/auth"
	"im-server/internal/pkg/response"
)

// tokenFor 直接用测试密钥给某个账号签发令牌（管理员账号在 fakeRepo 里没有密码，不走登录）
func tokenFor(t *testing.T, userID int64) string {
	t.Helper()
	jwtMgr, err := auth.NewJWTManager(testJWTSecret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := jwtMgr.Issue(userID)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// postFrom 从指定 IP 发 POST 请求（c.ClientIP() 取 RemoteAddr）
func postFrom(r *gin.Engine, ip, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":40000"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func expectCode(t *testing.T, step string, w *httptest.ResponseRecorder, status, code int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("%s：status = %d, want %d, body = %s", step, w.Code, status, w.Body.String())
	}
	if got, _, _ := decode[any](t, w); got != code {
		t.Fatalf("%s：code = %d, want %d, body = %s", step, got, code, w.Body.String())
	}
}

// 注册页的完整流程：验证（冻结）→ 别人验证被拒 → 续期 → 提交注册申请 → 码失效
func TestHandler_InviteReservationFlow(t *testing.T) {
	r, env := newTestRouterEnv(t)
	code := env.newInvite(t, InviteOptions{}).Code

	w := postFrom(r, "192.0.2.10", "/api/v1/auth/invite/verify", fmt.Sprintf(`{"inviteCode":%q}`, strings.ToLower(code)))
	expectCode(t, "验证", w, http.StatusOK, response.CodeOk)
	_, _, res := decode[reservationResp](t, w)
	if res.ReservationToken == "" || res.ExpiresAt.IsZero() || res.ReservedUntil.IsZero() {
		t.Fatalf("验证结果 = %s", w.Body.String())
	}
	assertUTCField(t, w.Body.String(), "expiresAt")

	w = postFrom(r, "192.0.2.11", "/api/v1/auth/invite/verify", fmt.Sprintf(`{"inviteCode":%q}`, code))
	expectCode(t, "别人验证", w, http.StatusConflict, response.CodeInviteInUse)

	hold := fmt.Sprintf(`{"inviteCode":%q,"reservationToken":%q}`, code, res.ReservationToken)
	w = postFrom(r, "192.0.2.10", "/api/v1/auth/invite/renew", hold)
	expectCode(t, "续期", w, http.StatusOK, response.CodeOk)
	if strings.Contains(w.Body.String(), "reservationToken") {
		t.Errorf("续期不应再返回令牌: %s", w.Body.String())
	}

	// 没带令牌、令牌不对：409 20007
	expectCode(t, "不带令牌提交", postJSON(r, "/api/v1/auth/register",
		fmt.Sprintf(`{"username":"eve","password":"Passw0rd","inviteCode":%q}`, code)),
		http.StatusConflict, response.CodeReservationLost)
	expectCode(t, "令牌不对", postJSON(r, "/api/v1/auth/register",
		fmt.Sprintf(`{"username":"eve","password":"Passw0rd","inviteCode":%q,"reservationToken":"WRONG"}`, code)),
		http.StatusConflict, response.CodeReservationLost)

	w = postJSON(r, "/api/v1/auth/register",
		fmt.Sprintf(`{"username":"alice","password":"Passw0rd","inviteCode":%q,"reservationToken":%q}`, code, res.ReservationToken))
	expectCode(t, "提交注册", w, http.StatusOK, response.CodeOk)

	// 提交之后码立即失效：续期失败、再验证无效
	expectCode(t, "提交后续期", postFrom(r, "192.0.2.10", "/api/v1/auth/invite/renew", hold),
		http.StatusConflict, response.CodeReservationLost)
	env.repo.advance(time.Minute + time.Second)
	expectCode(t, "提交后再验证", postFrom(r, "192.0.2.12", "/api/v1/auth/invite/verify", fmt.Sprintf(`{"inviteCode":%q}`, code)),
		http.StatusBadRequest, response.CodeInvalidInvite)
}

func TestHandler_InviteRelease(t *testing.T) {
	r, env := newTestRouterEnv(t)
	inv := env.newInvite(t, InviteOptions{})
	token := env.reserve(t, inv.Code)

	// 软释放（页面卸载时 sendBeacon 发出）：保留 15 秒
	w := postFrom(r, "192.0.2.20", "/api/v1/auth/invite/release",
		fmt.Sprintf(`{"inviteCode":%q,"reservationToken":%q,"soft":true}`, inv.Code, token))
	expectCode(t, "软释放", w, http.StatusOK, response.CodeOk)
	if env.state(inv.ID) != InviteReserved {
		t.Fatalf("软释放后 15 秒内仍是冻结中，got %s", env.state(inv.ID))
	}
	// 立即释放（在站内离开注册页）
	w = postFrom(r, "192.0.2.20", "/api/v1/auth/invite/release",
		fmt.Sprintf(`{"inviteCode":%q,"reservationToken":%q}`, inv.Code, token))
	expectCode(t, "立即释放", w, http.StatusOK, response.CodeOk)
	if env.state(inv.ID) != InviteActive {
		t.Errorf("立即释放后 state = %s", env.state(inv.ID))
	}
	// 令牌不对、码不存在：同样返回成功（幂等，不透露任何信息）
	expectCode(t, "令牌不对", postFrom(r, "192.0.2.20", "/api/v1/auth/invite/release", `{"inviteCode":"ZZZZZZ","reservationToken":"x"}`),
		http.StatusOK, response.CodeOk)
	expectCode(t, "请求体错误", postFrom(r, "192.0.2.20", "/api/v1/auth/invite/release", `{bad`),
		http.StatusBadRequest, response.CodeBadRequest)
}

func TestHandler_InviteVerify_Limits(t *testing.T) {
	r, env := newTestRouterEnv(t)
	verify := func(ip, code string) *httptest.ResponseRecorder {
		return postFrom(r, ip, "/api/v1/auth/invite/verify", fmt.Sprintf(`{"inviteCode":%q}`, code))
	}

	expectCode(t, "格式不对", verify("192.0.2.30", "ABC"), http.StatusBadRequest, response.CodeInvalidInvite)

	// 同一个码：解冻后 1 分钟内不能再验证
	inv := env.newInvite(t, InviteOptions{})
	w := verify("192.0.2.31", inv.Code)
	_, _, res := decode[reservationResp](t, w)
	if err := env.invites.Release(t.Context(), inv.Code, res.ReservationToken, false); err != nil {
		t.Fatal(err)
	}
	w = verify("192.0.2.32", inv.Code)
	expectCode(t, "1 分钟内再验证", w, http.StatusTooManyRequests, response.CodeTooManyRequests)
	if _, msg, _ := decode[any](t, w); msg != msgVerifyTooFrequent {
		t.Errorf("msg = %q", msg)
	}

	// 同一个 IP：24 小时内最多 10 次（不论码是否有效）
	const ip = "192.0.2.40"
	for i := range 10 {
		expectCode(t, fmt.Sprintf("第 %d 次猜码", i+1), verify(ip, fmt.Sprintf("ZZZZ%02d", i)), http.StatusBadRequest, response.CodeInvalidInvite)
	}
	w = verify(ip, env.newInvite(t, InviteOptions{}).Code)
	expectCode(t, "第 11 次", w, http.StatusTooManyRequests, response.CodeTooManyRequests)
	if _, msg, _ := decode[any](t, w); msg != msgVerifyIPLimit {
		t.Errorf("msg = %q", msg)
	}
}

// 管理员：/me 返回 canInvite=true；生成 → 有人验证并注册 → 列表里能看到使用者和验证记录 → 作废
func TestHandler_Invites_AdminFlow(t *testing.T) {
	r, env := newTestRouterEnv(t)
	admin := tokenFor(t, testAdminID)

	w := doRequest(r, http.MethodGet, "/api/v1/me", "", admin)
	if _, _, me := decode[userResp](t, w); !me.CanInvite {
		t.Fatalf("管理员 canInvite 应为 true: %s", w.Body.String())
	}

	w = doRequest(r, http.MethodPost, "/api/v1/invites", `{"note":"给小王"}`, admin)
	if w.Code != http.StatusOK {
		t.Fatalf("生成 status = %d, body = %s", w.Code, w.Body.String())
	}
	_, _, inv := decode[inviteResp](t, w)
	if len(inv.Code) != 6 || inv.Note != "给小王" || inv.State != InviteActive {
		t.Fatalf("生成结果 = %s", w.Body.String())
	}
	assertUTCField(t, w.Body.String(), "expiresAt")
	for _, want := range []string{`"usedAt":null`, `"revokedAt":null`, `"reservedUntil":null`, `"usedBy":null`, `"verifications":[]`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("响应里应有 %s: %s", want, w.Body.String())
		}
	}

	// 不带请求体也能生成（不填备注）
	w = doRequest(r, http.MethodPost, "/api/v1/invites", "", admin)
	if w.Code != http.StatusOK {
		t.Errorf("空请求体 status = %d, body = %s", w.Code, w.Body.String())
	}

	// 有人验证了第二个码（冻结中），第一个码被用来注册
	w = postFrom(r, "192.0.2.50", "/api/v1/auth/invite/verify", fmt.Sprintf(`{"inviteCode":%q}`, inv.Code))
	_, _, res := decode[reservationResp](t, w)
	w = postJSON(r, "/api/v1/auth/register",
		fmt.Sprintf(`{"username":"wang","password":"Passw0rd","inviteCode":%q,"reservationToken":%q}`, inv.Code, res.ReservationToken))
	_, _, reg := decode[registerResp](t, w)

	w = doRequest(r, http.MethodGet, "/api/v1/invites", "", admin)
	_, _, list := decode[inviteListResp](t, w)
	if len(list.Items) != 2 {
		t.Fatalf("列表 = %s", w.Body.String())
	}
	used := list.Items[1] // 最新的在前，先生成的在后
	if used.ID != inv.ID || used.State != InviteUsed || used.UsedAt == nil || used.UsedBy == nil ||
		used.UsedBy.ID != reg.ID || used.UsedBy.Status != StatusPending {
		t.Errorf("溯源信息不对: %+v", used)
	}
	if len(used.Verifications) != 1 || used.Verifications[0].IP != "192.0.2.50" || used.Verifications[0].Result != VerifyReserved {
		t.Errorf("验证记录不对: %+v", used.Verifications)
	}

	fresh := list.Items[0]
	env.reserve(t, fresh.Code)
	w = doRequest(r, http.MethodGet, "/api/v1/invites", "", admin)
	_, _, list = decode[inviteListResp](t, w)
	if list.Items[0].State != InviteReserved || list.Items[0].ReservedUntil == nil {
		t.Errorf("冻结中的码: %+v", list.Items[0])
	}

	w = doRequest(r, http.MethodPost, fmt.Sprintf("/api/v1/invites/%d/revoke", fresh.ID), "", admin)
	if _, _, d := decode[inviteResp](t, w); d.State != InviteRevoked || d.RevokedAt == nil || d.ReservedUntil != nil {
		t.Errorf("作废结果 = %s", w.Body.String())
	}
}

func TestHandler_Invites_Errors(t *testing.T) {
	r, env := newTestRouterEnv(t)
	admin := tokenFor(t, testAdminID)
	alice := mustRegisterActive(t, r, env, "alice", "Passw0rd")
	normal := tokenFor(t, alice.ID)

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		token      string
		wantStatus int
		wantCode   int
	}{
		{"普通账号不能生成", http.MethodPost, "/api/v1/invites", `{}`, normal, http.StatusForbidden, response.CodeForbidden},
		{"未登录", http.MethodPost, "/api/v1/invites", `{}`, "", http.StatusUnauthorized, response.CodeUnauthorized},
		{"备注太长", http.MethodPost, "/api/v1/invites", fmt.Sprintf(`{"note":%q}`, strings.Repeat("字", 51)), admin,
			http.StatusBadRequest, response.CodeInvalidParam},
		{"请求体格式错误", http.MethodPost, "/api/v1/invites", `{bad`, admin, http.StatusBadRequest, response.CodeBadRequest},
		{"作废不存在的码", http.MethodPost, "/api/v1/invites/999/revoke", "", admin, http.StatusNotFound, response.CodeInviteNotFound},
		{"作废别人的码", http.MethodPost, "/api/v1/invites/1/revoke", "", normal, http.StatusNotFound, response.CodeInviteNotFound},
		{"ID 不是数字", http.MethodPost, "/api/v1/invites/abc/revoke", "", admin, http.StatusBadRequest, response.CodeBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := doRequest(r, tt.method, tt.path, tt.body, tt.token)
			expectCode(t, tt.name, w, tt.wantStatus, tt.wantCode)
		})
	}

	// 普通账号的列表是空数组（不是 null），也看不到管理员的码
	w := doRequest(r, http.MethodGet, "/api/v1/invites", "", normal)
	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Errorf("普通账号的列表 = %s", w.Body.String())
	}
}
