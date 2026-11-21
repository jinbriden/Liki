package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func main() {
	dsn := os.Getenv("DATABASE_DSN") // 读取数据库连接配置
	if dsn == "" {
		dsn = "host=localhost user=im password=im123 dbname=im port=5432 sslmode=disable"
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
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

	if err := r.Run("127.0.0.1:8080"); err != nil {
		slog.Error("服务启动失败", "err", err)
		os.Exit(1)
	}
}
