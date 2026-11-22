package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"im-server/internal/friend"
	"im-server/internal/pkg/auth"
	"im-server/internal/user"
)

// tokenTTL 访问令牌有效期
const tokenTTL = 3 * 24 * time.Hour

func main() {
	// JWT 密钥没有默认值：带着写死在代码里的密钥部署，任何人都能伪造令牌。缺失时直接拒绝启动。
	jwtMgr, err := auth.NewJWTManager(os.Getenv("JWT_SECRET"), tokenTTL)
	if err != nil {
		slog.Error("JWT 配置错误，请设置环境变量 JWT_SECRET（至少 32 字节）", "err", err)
		os.Exit(1)
	}

	dsn := os.Getenv("DATABASE_DSN")
	if dsn == "" {
		dsn = "host=localhost user=im password=im123 dbname=im port=5432 sslmode=disable"
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{TranslateError: true})
	if err != nil {
		slog.Error("连接数据库失败", "err", err)
		os.Exit(1)
	}

	r := gin.Default()
	r.GET("/health", func(c *gin.Context) {
		sqlDB, err := db.DB()
		if err != nil || sqlDB.Ping() != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db down"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 手动组装依赖：repository → service → handler
	userRepo := user.NewRepository(db)
	userSvc := user.NewService(userRepo, jwtMgr, user.RandomUserID)
	userHandler := user.NewHandler(userSvc)

	friendRepo := friend.NewRepository(db)
	friendSvc := friend.NewService(friendRepo, userSvc) // *user.Service 满足 friend.UserDirectory 接口
	friendHandler := friend.NewHandler(friendSvc)

	api := r.Group("/api/v1")                           // 公开接口
	protected := api.Group("", auth.Middleware(jwtMgr)) // 需要登录的接口
	userHandler.RegisterRoutes(api, protected)
	friendHandler.RegisterRoutes(api, protected)

	if err := r.Run("127.0.0.1:8080"); err != nil {
		slog.Error("服务启动失败", "err", err)
		os.Exit(1)
	}
}
