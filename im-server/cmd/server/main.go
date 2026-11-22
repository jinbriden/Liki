package main

import (
	"context"
	"errors"
	"fmt"
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
	"im-server/internal/pkg/ratelimit"
	"im-server/internal/pkg/reqctx"
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

// 限流规则（NFR-05）。令牌桶：平均每 Per 最多 Limit 次，最多一次性突发 Burst 次（不填等于 Limit）
var (
	registerPerIP        = ratelimit.Rule{Name: "register_ip", Limit: 10, Per: time.Hour}
	loginPerIP           = ratelimit.Rule{Name: "login_ip", Limit: 20, Per: time.Minute}
	loginPerAccount      = ratelimit.Rule{Name: "login_account", Limit: 5, Per: time.Minute} // 防止针对单个账号暴力破解
	friendRequestPerUser = ratelimit.Rule{Name: "friend_request_user", Limit: 30, Per: time.Hour}
	wsTicketPerUser      = ratelimit.Rule{Name: "ws_ticket_user", Limit: 30, Per: time.Minute} // 防止客户端重连死循环
	sendPerUser          = ratelimit.Rule{Name: "send_user", Limit: 5, Per: time.Second, Burst: 10}
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
	// 不信任任何代理：c.ClientIP() 直接取 TCP 连接的对端地址，客户端伪造的 X-Forwarded-For 不起作用，
	// 否则按 IP 限流可以被轻易绕过（gin 默认信任所有代理）。开发时请求都经过 Vite 代理，IP 都是 127.0.0.1；
	// 部署到 Nginx 后面时，这里改成信任 Nginx 的地址。
	if err := r.SetTrustedProxies(nil); err != nil {
		return err
	}
	// 顺序：日志中间件在最外层（生成 request_id，最后记录访问日志），Recovery 在里面（panic 也能被访问日志记到 500）；
	// reqctx 让写请求不因客户端断开而中途取消（避免"数据库已提交、接口却返回 500、推送被跳过"）
	r.Use(logger.Middleware(), logger.Recovery(), reqctx.Middleware())
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
	userSvc := user.NewService(userRepo, jwtMgr, user.RandomUserID,
		user.WithLoginLimiter(ratelimit.NewMemory(loginPerAccount)))
	userHandler := user.NewHandler(userSvc)

	friendRepo := friend.NewRepository(db)
	friendSvc := friend.NewService(friendRepo, userSvc, friend.NewNotifier(gw)) // *user.Service 满足 friend.UserDirectory 接口
	friendHandler := friend.NewHandler(friendSvc)

	messageRepo := message.NewRepository(db)
	// *friend.Service 满足 message.FriendChecker，*user.Service 满足 message.UserDirectory
	messageSvc := message.NewService(messageRepo, friendSvc, userSvc, message.NewNotifier(gw),
		message.WithSendLimiter(ratelimit.NewMemory(sendPerUser)))
	messageHandler := message.NewHandler(messageSvc)
	messageHandler.RegisterPackets(gw)

	// 限流中间件按"方法 路由模板"挑规则，挂在路由组上，业务模块注册路由时不用关心限流。
	// 按 IP 的挂在公开接口组；按用户的挂在鉴权之后（这时才知道是谁）。
	// 中间件要在创建路由组时传入：gin 在注册路由那一刻把组上的中间件拷进路由，之后再 Use 不会生效
	limit := func(r ratelimit.Rule, key ratelimit.KeyFunc) gin.HandlerFunc {
		return ratelimit.Middleware(ratelimit.NewMemory(r), r.Name, key)
	}
	ipRules := map[string][]gin.HandlerFunc{
		"POST /api/v1/auth/register": {limit(registerPerIP, ratelimit.ByIP)},
		"POST /api/v1/auth/login":    {limit(loginPerIP, ratelimit.ByIP)}, // 按账号的限制在 user.Service 里
	}
	userRules := map[string][]gin.HandlerFunc{
		"POST /api/v1/friend-requests": {limit(friendRequestPerUser, ratelimit.ByUser)},
		"POST /api/v1/ws/ticket":       {limit(wsTicketPerUser, ratelimit.ByUser)},
	}

	api := r.Group("/api/v1", ratelimit.Routes(ipRules))                             // 公开接口
	protected := api.Group("", auth.Middleware(jwtMgr), ratelimit.Routes(userRules)) // 需要登录的接口
	userHandler.RegisterRoutes(api, protected)
	friendHandler.RegisterRoutes(api, protected)
	messageHandler.RegisterRoutes(api, protected)
	gw.RegisterRoutes(api, protected)
	// 限流规则按路由字符串匹配，路由改名后规则会悄悄失效。启动时核对一遍，对不上就拒绝启动
	if err := checkRoutes(r, ipRules, userRules); err != nil {
		return err
	}

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

// checkRoutes 确认限流规则里的每个"方法 路由"都真的注册了
func checkRoutes(r *gin.Engine, rules ...map[string][]gin.HandlerFunc) error {
	registered := map[string]bool{}
	for _, rt := range r.Routes() {
		registered[rt.Method+" "+rt.Path] = true
	}
	for _, m := range rules {
		for key := range m {
			if !registered[key] {
				return fmt.Errorf("rate limit rule for unknown route %q", key)
			}
		}
	}
	return nil
}
