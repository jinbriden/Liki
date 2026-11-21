package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"im-server/internal/pkg/response"
)

func TestMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m := newTestManager(t)
	validToken, _, err := m.Issue(1234567890)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	r := gin.New()
	r.GET("/protected", Middleware(m), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"uid": UserID(c)})
	})

	tests := []struct {
		name       string
		header     string // 空字符串表示不带 Authorization 头
		wantStatus int
	}{
		{"有效令牌", "Bearer " + validToken, http.StatusOK},
		{"scheme 小写也接受", "bearer " + validToken, http.StatusOK},
		{"没有 Authorization 头", "", http.StatusUnauthorized},
		{"缺少 Bearer 前缀", validToken, http.StatusUnauthorized},
		{"错误的 scheme", "Basic " + validToken, http.StatusUnauthorized},
		{"Bearer 后面为空", "Bearer ", http.StatusUnauthorized},
		{"只有空格", "Bearer    ", http.StatusUnauthorized},
		{"无效令牌", "Bearer not-a-jwt", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/protected", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantStatus == http.StatusOK {
				var got struct{ UID int64 }
				_ = json.Unmarshal(w.Body.Bytes(), &got)
				if got.UID != 1234567890 {
					t.Errorf("handler 拿到的 uid = %d, want 1234567890", got.UID)
				}
				return
			}
			var body response.Body
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if body.Code != response.CodeUnauthorized {
				t.Errorf("code = %d, want %d", body.Code, response.CodeUnauthorized)
			}
		})
	}
}
