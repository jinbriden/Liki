// Package logger 初始化全局结构化日志（标准库 slog），并提供"把字段挂到 context 上"的能力。
//
// 用法：
//   - 启动时调用 Setup，之后各处直接用 slog.InfoContext(ctx, ...) / slog.ErrorContext(ctx, ...)。
//   - HTTP 中间件、鉴权中间件、WebSocket 网关通过 WithAttrs 把 request_id、user_id、conn_id 挂到 ctx 上，
//     之后凡是带着这个 ctx 打的日志都会自动带上这些字段，service 层不需要手动传。
//     作用类似 Java 里 SLF4J 的 MDC，区别是 Go 没有线程局部变量，字段跟着 ctx 显式传递。
//
// 安全约定：日志里不能出现密码、令牌、WebSocket 票据和消息正文。
package logger

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Config 日志配置，由环境变量注入（见 ConfigFromEnv）
type Config struct {
	Level   slog.Level // 最低输出级别
	Format  string     // 控制台格式：text（便于人看）或 json
	File    string     // 日志文件路径，空字符串表示只输出到控制台；文件里固定写 JSON，便于 grep / jq 检索
	LogSQL  bool       // 是否记录每一条 SQL（参数用占位符代替，不记录参数值）
	SlowSQL time.Duration
	// NoConsole 不输出到控制台，只写文件（命令行工具用：控制台留给命令本身的输出，审计日志照样进文件）
	NoConsole bool
}

const defaultLogFile = "logs/im-server.log"

// ConfigFromEnv 读取环境变量：
//
//	LOG_LEVEL   debug | info | warn | error，默认 debug（开发阶段）
//	LOG_FORMAT  text | json，控制台输出格式，默认 text
//	LOG_FILE    日志文件路径，默认 logs/im-server.log（相对于启动目录）；设为空字符串则不写文件
//	LOG_SQL     true 时记录所有 SQL；默认只记录慢查询（>200ms）和出错的 SQL
func ConfigFromEnv() (Config, error) {
	cfg := Config{Level: slog.LevelDebug, Format: "text", File: defaultLogFile, SlowSQL: 200 * time.Millisecond}

	if v := os.Getenv("LOG_LEVEL"); v != "" {
		// slog.Level 实现了 UnmarshalText，能解析 "debug"、"INFO"、"warn" 等（不区分大小写）
		if err := cfg.Level.UnmarshalText([]byte(v)); err != nil {
			return cfg, fmt.Errorf("invalid LOG_LEVEL %q: %w", v, err)
		}
	}
	if v := os.Getenv("LOG_FORMAT"); v != "" {
		v = strings.ToLower(v)
		if v != "text" && v != "json" {
			return cfg, fmt.Errorf("invalid LOG_FORMAT %q: want text or json", v)
		}
		cfg.Format = v
	}
	// LookupEnv 能区分"没设置"和"设置为空字符串"：前者用默认文件，后者表示不写文件
	if v, ok := os.LookupEnv("LOG_FILE"); ok {
		cfg.File = v
	}
	cfg.LogSQL = strings.EqualFold(os.Getenv("LOG_SQL"), "true") || os.Getenv("LOG_SQL") == "1"
	return cfg, nil
}

// Setup 按配置创建 logger 并设为 slog 的默认 logger。
// 返回的 close 在程序退出前调用，用来关闭日志文件。
func Setup(cfg Config) (close func() error, err error) {
	opts := &slog.HandlerOptions{Level: cfg.Level}

	var console slog.Handler
	if cfg.Format == "json" {
		console = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		console = slog.NewTextHandler(os.Stdout, opts)
	}

	handler := console
	if cfg.NoConsole {
		handler = slog.DiscardHandler
	}
	close = func() error { return nil }
	if cfg.File != "" {
		f, err := openLogFile(cfg.File)
		if err != nil {
			return nil, err
		}
		file := slog.NewJSONHandler(f, opts)
		if cfg.NoConsole {
			handler = file
		} else {
			// MultiHandler（Go 1.26+）把同一条日志分发给多个 handler：控制台给人看，文件给工具查
			handler = slog.NewMultiHandler(console, file)
		}
		close = f.Close
	}

	slog.SetDefault(slog.New(contextHandler{handler}))
	return close, nil
}

func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	// O_APPEND：重启后接着写，不覆盖之前的日志。0o640：日志可能含用户 ID 等信息，不给其他用户读
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	return f, nil
}

// ---------- context 上的日志字段 ----------

// ctxKey 用私有类型做 key，其他包无法构造同样的 key，避免冲突
type ctxKey struct{}

// WithAttrs 返回一个新 ctx，在原有字段基础上追加 args（写法同 slog：key, value, key, value...）。
// 原 ctx 不受影响（ctx 是不可变的，每次 With 都生成新的一层）。
func WithAttrs(ctx context.Context, args ...any) context.Context {
	if len(args) == 0 {
		return ctx
	}
	// 借用 slog.Record 把 "k1", v1, "k2", v2 这种写法转成 []slog.Attr，和 slog.Info 的参数规则完全一致
	r := slog.NewRecord(time.Time{}, 0, "", 0)
	r.Add(args...)

	attrs := slices.Clone(attrsFrom(ctx)) // 复制一份再追加，不能改到父 ctx 持有的切片
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	return context.WithValue(ctx, ctxKey{}, attrs)
}

func attrsFrom(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}
	attrs, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	return attrs
}

// contextHandler 包装真正的 handler：每条日志输出前，把 ctx 上挂的字段补进去。
// 只有用 slog.XxxContext(ctx, ...) 打的日志才能拿到 ctx；slog.Info(...) 不带 ctx，也就没有这些字段。
type contextHandler struct {
	slog.Handler // 嵌入：Enabled 等方法直接沿用内层 handler 的实现
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs := attrsFrom(ctx); len(attrs) > 0 {
		r = r.Clone() // Record 内部的切片可能和调用方共享，修改前先复制
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs / WithGroup 必须重写：否则 slog.With(...) 返回的是内层 handler，会丢掉 contextHandler 这一层
func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}
