package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"im-server/internal/friend"
	"im-server/internal/gateway"
	"im-server/internal/message"
	"im-server/internal/pkg/auth"
	"im-server/internal/pkg/logger"
	"im-server/internal/user"
)

const (
	listenAddr = "127.0.0.1:8080"
	// tokenTTL 访问令牌有效期
	tokenTTL = 3 * 24 * time.Hour
	// wsTicketTTL WebSocket 建连票据有效期：前端拿到票据后立刻建连，30 秒足够
	wsTicketTTL = 30 * time.Second
	// shutdownTimeout 收到退出信号后，等待进行中的 HTTP 请求完成的最长时间
	shutdownTimeout = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("服务异常退出", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// 日志最先初始化，后面所有步骤（包括启动失败）都能记录下来
	logCfg, err := logger.ConfigFromEnv()
	if err != nil {
		return err
	}
	closeLog, err := logger.Setup(logCfg)
	if err != nil {
		return err
	}
	defer closeLog()
	slog.Info("logger ready", "level", logCfg.Level.String(), "file", logCfg.File, "log_sql", logCfg.LogSQL)

	// JWT 密钥没有默认值：带着写死在代码里的密钥部署，任何人都能伪造令牌。缺失时直接拒绝启动。
	jwtMgr, err := auth.NewJWTManager(os.Getenv("JWT_SECRET"), tokenTTL)
	if err != nil {
		slog.Error("JWT 配置错误，请设置环境变量 JWT_SECRET（至少 32 字节）")
		return err
	}

	dsn := os.Getenv("DATABASE_DSN")
	if dsn == "" {
		dsn = "host=localhost user=im password=im123 dbname=im port=5432 sslmode=disable"
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		TranslateError: true,
		Logger:         newGormLogger(logCfg),
	})
	if err != nil {
		slog.Error("连接数据库失败")
		return err
	}

	r := gin.New()
	// 顺序：日志中间件在最外层（生成 request_id，最后记录访问日志），Recovery 在里面（panic 也能被访问日志记到 500）
	r.Use(logger.Middleware(), logger.Recovery())
	r.GET("/health", func(c *gin.Context) {
		sqlDB, err := db.DB()
		if err != nil || sqlDB.PingContext(c.Request.Context()) != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db down"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 手动组装依赖：repository → service → handler。
	// 网关最先创建：业务模块推送时要用它（Pusher），它分发数据包时又要用业务模块的处理函数。
	// 两边都只依赖对方的接口，在这里接上，不会形成包之间的循环依赖（类似 Spring 里通过接口注入解开循环依赖）。
	gw := gateway.New(gateway.NewMemoryTicketStore(wsTicketTTL), gateway.NewMemoryPresence())

	userRepo := user.NewRepository(db)
	userSvc := user.NewService(userRepo, jwtMgr, user.RandomUserID)
	userHandler := user.NewHandler(userSvc)

	friendRepo := friend.NewRepository(db)
	friendSvc := friend.NewService(friendRepo, userSvc, friend.NewNotifier(gw)) // *user.Service 满足 friend.UserDirectory 接口
	friendHandler := friend.NewHandler(friendSvc)

	messageRepo := message.NewRepository(db)
	// *friend.Service 满足 message.FriendChecker，*user.Service 满足 message.UserDirectory
	messageSvc := message.NewService(messageRepo, friendSvc, userSvc, message.NewNotifier(gw))
	messageHandler := message.NewHandler(messageSvc)
	messageHandler.RegisterPackets(gw)

	api := r.Group("/api/v1")                           // 公开接口
	protected := api.Group("", auth.Middleware(jwtMgr)) // 需要登录的接口
	userHandler.RegisterRoutes(api, protected)
	friendHandler.RegisterRoutes(api, protected)
	messageHandler.RegisterRoutes(api, protected)
	gw.RegisterRoutes(api, protected)

	srv := &http.Server{Addr: listenAddr, Handler: r, ReadHeaderTimeout: 10 * time.Second}

	// 优雅关闭：收到 Ctrl+C（SIGINT）或 SIGTERM 后，不再接受新请求，等进行中的请求处理完，再关闭所有 WebSocket 连接
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("server listening", "addr", listenAddr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err // 端口被占用等启动错误
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	// Shutdown 不管已经升级成 WebSocket 的连接（它们已经脱离了 http.Server），所以网关要单独关闭
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Warn("http shutdown", "err", err)
	}
	gw.Close()
	slog.Info("server stopped")
	return nil
}

// newGormLogger SQL 日志也走 slog：带着 ctx 的查询会自动带上 request_id、user_id，便于和请求日志对上。
// ParameterizedQueries 只记录带 ? 占位符的 SQL，不记录参数值（参数里可能有密码哈希、消息正文）。
func newGormLogger(cfg logger.Config) gormlogger.Interface {
	level := gormlogger.Warn // 默认只记录慢查询和出错的 SQL
	if cfg.LogSQL {
		level = gormlogger.Info
	}
	inner := gormlogger.NewSlogLogger(slog.Default(), gormlogger.Config{
		SlowThreshold:             cfg.SlowSQL,
		LogLevel:                  level,
		IgnoreRecordNotFoundError: true, // "查不到"是正常业务分支（如用户不存在），不当作错误
		ParameterizedQueries:      true,
	})
	return expectedErrorLogger{inner}
}

// expectedErrorLogger 唯一约束、外键约束冲突在本项目里是被 repository 翻译成业务错误的正常分支
// （重复的好友申请、消息去重、目标用户不存在），不应该以 ERROR 级别记成"SQL 出错"。
// 这类 SQL 按成功处理（只在 LOG_SQL=true 时出现），由业务层决定要不要记录。
type expectedErrorLogger struct {
	gormlogger.Interface
}

func (l expectedErrorLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if errors.Is(err, gorm.ErrDuplicatedKey) || errors.Is(err, gorm.ErrForeignKeyViolated) {
		err = nil
	}
	l.Interface.Trace(ctx, begin, fc, err)
}

// LogMode 必须重写，否则 db.Debug() 等调用返回的是内层 logger，会丢掉这层包装
func (l expectedErrorLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	return expectedErrorLogger{l.Interface.LogMode(level)}
}
