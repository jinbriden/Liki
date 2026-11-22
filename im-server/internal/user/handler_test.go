package user

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"im-server/internal/pkg/auth"
	"im-server/internal/pkg/response"
)

const testJWTSecret = "test-secret-0123456789-abcdefghijklmnop"

// newTestRouter 用真实的 JWTManager 和鉴权中间件、假的 repository，按 main.go 的方式组装路由
func newTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	jwtMgr, err := auth.NewJWTManager(testJWTSecret, 72*time.Hour)
	if err != nil {
		t.Fatalf("NewJWTManager: %v", err)
	}
	h := NewHandler(NewService(newFakeRepo(), jwtMgr, counterIDs()))

	r := gin.New()
	api := r.Group("/api/v1")
	h.RegisterRoutes(api, api.Group("", auth.Middleware(jwtMgr)))
	return r
}

func doRequest(r *gin.Engine, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func postJSON(r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	return doRequest(r, http.MethodPost, path, body, "")
}

// decode 解析统一响应格式，data 解析到 T
func decode[T any](t *testing.T, w *httptest.ResponseRecorder) (code int, msg string, data T) {
	t.Helper()
	var body struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data T      `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v, body = %s", err, w.Body.String())
	}
	return body.Code, body.Msg, body.Data
}

func mustRegister(t *testing.T, r *gin.Engine, username, password string) userResp {
	t.Helper()
	w := postJSON(r, "/api/v1/auth/register", fmt.Sprintf(`{"username":%q,"password":%q}`, username, password))
	if w.Code != http.StatusOK {
		t.Fatalf("注册失败: %d %s", w.Code, w.Body.String())
	}
	_, _, u := decode[userResp](t, w)
	return u
}

// ---------- 注册 ----------

// 同名注册两次都成功，返回不同的 10 位 ID；用户名大小写原样返回；响应里不含密码相关字段
func TestHandler_Register_OK(t *testing.T) {
	r := newTestRouter(t)
	body := `{"username":"Alice","password":"Passw0rd"}`

	var ids []int64
	for range 2 {
		w := postJSON(r, "/api/v1/auth/register", body)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body = %s", w.Code, w.Body.String())
		}
		if strings.Contains(strings.ToLower(w.Body.String()), "password") {
			t.Fatalf("响应中不应出现 password 字段: %s", w.Body.String())
		}
		code, _, u := decode[userResp](t, w)
		if code != response.CodeOk || u.Username != "Alice" || u.ID < minUserID || u.ID > maxUserID {
			t.Fatalf("unexpected resp: %s", w.Body.String())
		}
		ids = append(ids, u.ID)
	}
	if ids[0] == ids[1] {
		t.Fatalf("两次同名注册得到相同 ID %d", ids[0])
	}
}

// 请求体无法解析返回 10002；业务规则不满足返回 10001 + 对应字段的提示
func TestHandler_Register_Errors(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode int
		wantMsg  string
	}{
		{"空body", ``, response.CodeBadRequest, msgBadRequest},
		{"JSON语法错误", `{bad`, response.CodeBadRequest, msgBadRequest},
		{"JSON被截断", `{"username":"dave","password":"Passw0rd"`, response.CodeBadRequest, msgBadRequest},
		{"字段类型错误", `{"username":123,"password":"Passw0rd"}`, response.CodeBadRequest, msgBadRequest},
		{"顶层不是对象", `[]`, response.CodeBadRequest, msgBadRequest},

		{"缺少用户名", `{"password":"Passw0rd"}`, response.CodeInvalidParam, msgInvalidUsername},
		{"用户名不合规", `{"username":"a_b","password":"Passw0rd"}`, response.CodeInvalidParam, msgInvalidUsername},
		{"缺少密码", `{"username":"dave"}`, response.CodeInvalidParam, msgInvalidPassword},
		{"密码缺大写", `{"username":"dave","password":"passw0rd"}`, response.CodeInvalidParam, msgInvalidPassword},
		{"密码过长", `{"username":"dave","password":"Passw0rd` + strings.Repeat("x", 17) + `"}`, response.CodeInvalidParam, msgInvalidPassword},
		{"用户名和密码都不合规时先报用户名", `{"username":"ab","password":"1"}`, response.CodeInvalidParam, msgInvalidUsername},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := postJSON(newTestRouter(t), "/api/v1/auth/register", tt.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
			}
			code, msg, _ := decode[any](t, w)
			if code != tt.wantCode || msg != tt.wantMsg {
				t.Errorf("got {code:%d, msg:%q}, want {code:%d, msg:%q}", code, msg, tt.wantCode, tt.wantMsg)
			}
		})
	}
}

// ---------- 登录 + /me ----------

// 完整流程：注册 → 登录拿令牌 → 带令牌访问 /me
func TestHandler_LoginThenMe(t *testing.T) {
	r := newTestRouter(t)
	reg := mustRegister(t, r, "Alice", "Passw0rd")

	w := postJSON(r, "/api/v1/auth/login", fmt.Sprintf(`{"userId":%d,"password":"Passw0rd"}`, reg.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("登录 status = %d, body = %s", w.Code, w.Body.String())
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "password") {
		t.Fatalf("登录响应中不应出现 password 字段: %s", w.Body.String())
	}
	_, _, login := decode[loginResp](t, w)
	if login.Token == "" || login.User.ID != reg.ID || login.User.Username != "Alice" {
		t.Fatalf("unexpected login resp: %s", w.Body.String())
	}
	if d := time.Until(login.ExpiresAt); d < 71*time.Hour || d > 72*time.Hour {
		t.Errorf("expiresAt 距现在 %s，应约为 72h", d)
	}

	w = doRequest(r, http.MethodGet, "/api/v1/me", "", login.Token)
	if w.Code != http.StatusOK {
		t.Fatalf("/me status = %d, body = %s", w.Code, w.Body.String())
	}
	_, _, me := decode[userResp](t, w)
	if me.ID != reg.ID || me.Username != "Alice" {
		t.Errorf("/me = %+v, want id=%d", me, reg.ID)
	}
	// 注册返回的 createdAt 必须和之后查询到的完全一致（精度、时区都相同）
	if !me.CreatedAt.Equal(reg.CreatedAt) {
		t.Errorf("createdAt 不一致: 注册=%s, /me=%s", reg.CreatedAt, me.CreatedAt)
	}
}

// 接口里的时间统一以 UTC 输出（RFC 3339 末尾为 Z），不受服务器时区影响
func TestHandler_TimesAreUTC(t *testing.T) {
	r := newTestRouter(t)
	w := postJSON(r, "/api/v1/auth/register", `{"username":"Alice","password":"Passw0rd"}`)
	_, _, reg := decode[userResp](t, w)
	assertUTCField(t, w.Body.String(), "createdAt")

	w = postJSON(r, "/api/v1/auth/login", fmt.Sprintf(`{"userId":%d,"password":"Passw0rd"}`, reg.ID))
	assertUTCField(t, w.Body.String(), "expiresAt")
	assertUTCField(t, w.Body.String(), "createdAt")
	_, _, login := decode[loginResp](t, w)

	w = doRequest(r, http.MethodGet, "/api/v1/me", "", login.Token)
	assertUTCField(t, w.Body.String(), "createdAt")
}

var timeFieldRe = regexp.MustCompile(`"(createdAt|expiresAt)":"([^"]*)"`)

func assertUTCField(t *testing.T, body, field string) {
	t.Helper()
	found := false
	for _, m := range timeFieldRe.FindAllStringSubmatch(body, -1) {
		if m[1] != field {
			continue
		}
		found = true
		if !strings.HasSuffix(m[2], "Z") {
			t.Errorf("%s = %q，应以 Z 结尾（UTC）", field, m[2])
		}
	}
	if !found {
		t.Errorf("响应中没有 %s 字段: %s", field, body)
	}
}

func TestHandler_Login_Errors(t *testing.T) {
	r := newTestRouter(t)
	reg := mustRegister(t, r, "Alice", "Passw0rd")

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   int
		wantMsg    string
	}{
		{"密码错误", fmt.Sprintf(`{"userId":%d,"password":"Wr0ngPass"}`, reg.ID),
			http.StatusUnauthorized, response.CodeInvalidCredentials, msgInvalidCredentials},
		{"用户不存在", `{"userId":9999999999,"password":"Passw0rd"}`,
			http.StatusUnauthorized, response.CodeInvalidCredentials, msgInvalidCredentials},
		{"缺少userId", `{"password":"Passw0rd"}`,
			http.StatusUnauthorized, response.CodeInvalidCredentials, msgInvalidCredentials},
		{"userId是字符串", fmt.Sprintf(`{"userId":"%d","password":"Passw0rd"}`, reg.ID),
			http.StatusBadRequest, response.CodeBadRequest, msgBadRequest},
		{"JSON语法错误", `{bad`,
			http.StatusBadRequest, response.CodeBadRequest, msgBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := postJSON(r, "/api/v1/auth/login", tt.body)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", w.Code, tt.wantStatus, w.Body.String())
			}
			code, msg, _ := decode[any](t, w)
			if code != tt.wantCode || msg != tt.wantMsg {
				t.Errorf("got {code:%d, msg:%q}, want {code:%d, msg:%q}", code, msg, tt.wantCode, tt.wantMsg)
			}
		})
	}
}

// M2 完成标准：未登录访问受保护接口返回 401
func TestHandler_Me_Unauthorized(t *testing.T) {
	r := newTestRouter(t)

	// 用同一个密钥给一个不存在的用户签发令牌：令牌有效但账号不存在
	jwtMgr, _ := auth.NewJWTManager(testJWTSecret, time.Hour)
	ghostToken, _, _ := jwtMgr.Issue(9999999999)

	for name, token := range map[string]string{
		"不带令牌":     "",
		"无效令牌":     "not-a-jwt",
		"账号不存在的令牌": ghostToken,
	} {
		t.Run(name, func(t *testing.T) {
			w := doRequest(r, http.MethodGet, "/api/v1/me", "", token)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401, body = %s", w.Code, w.Body.String())
			}
			if code, _, _ := decode[any](t, w); code != response.CodeUnauthorized {
				t.Errorf("code = %d, want %d", code, response.CodeUnauthorized)
			}
		})
	}
}
