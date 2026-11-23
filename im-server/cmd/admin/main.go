// im-admin 服务器上的管理命令行：注册审核（FR-22）和邀请码（FR-23）。
//
// 审核只在这里做，不提供接口和页面：能登录服务器（SSH 密钥）就是管理员身份的证明。
// 命令行生成的邀请码记在管理员账号（ADMIN_USER_ID）名下，和管理员在网页上生成的一样。
//
// 和 im-server 读同一套环境变量（线上用同一个 EnvironmentFile）：
//
//	DATABASE_DSN     必填
//	ADMIN_USER_ID    invite 子命令必填
//	PUBLIC_BASE_URL  可选，如 https://chat.example.com，设置后生成邀请码时同时打印邀请链接
//	LOG_FILE         审核、邀请码操作写入的日志文件，默认 logs/im-server.log（只写文件，不打到控制台）
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"im-server/internal/pkg/logger"
	account "im-server/internal/user" // 和标准库 os/user 重名，起个别名
)

const usage = `用法：im-admin <命令> [参数]

注册审核（FR-22）：
  list [pending|rejected]     列出待审核（默认）或已拒绝的账号，及其使用的邀请码
  approve <用户ID>...         批准账号（待审核、已拒绝的都可以批准）
  reject <用户ID>...          拒绝待审核的账号

邀请码（FR-23）：
  invite create [-expires 72h] [-note 备注]
                              生成邀请码（一个码只能提交一次注册申请，默认 3 天有效）
  invite list                 列出邀请码，最新的在前
  invite show <邀请码ID>      查看邀请码、用它注册的账号和验证记录
  invite revoke <邀请码ID>    作废还没使用的邀请码（冻结中的也可以）

环境变量：DATABASE_DSN（必填）、ADMIN_USER_ID（invite 必填）、PUBLIC_BASE_URL（可选）
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		os.Exit(1)
	}
}

// app 一次命令执行需要的依赖
type app struct {
	ctx     context.Context
	out     io.Writer
	review  *account.ReviewService
	invites *account.InviteService
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(out, usage)
		return nil
	}

	logCfg, err := logger.ConfigFromEnv()
	if err != nil {
		return err
	}
	logCfg.NoConsole = true
	closeLog, err := logger.Setup(logCfg)
	if err != nil {
		return err
	}
	defer closeLog()

	dsn := os.Getenv("DATABASE_DSN")
	if dsn == "" {
		return errors.New("请设置环境变量 DATABASE_DSN")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		TranslateError: true,
		Logger:         gormlogger.Default.LogMode(gormlogger.Silent), // 出错时由命令本身报告
	})
	if err != nil {
		return fmt.Errorf("连接数据库失败：%w", err)
	}

	var adminID int64
	if v := os.Getenv("ADMIN_USER_ID"); v != "" {
		id, ok := account.ParseID(v)
		if !ok {
			return fmt.Errorf("ADMIN_USER_ID %q 不是合法的 10 位用户 ID", v)
		}
		adminID = id
	}

	repo := account.NewRepository(db)
	a := &app{
		// 审计：日志里记下是谁、通过什么在操作（服务端的请求日志靠 request_id，这里靠 actor）
		ctx:     logger.WithAttrs(context.Background(), "actor", "im-admin", "os_user", osUser()),
		out:     out,
		review:  account.NewReviewService(repo),
		invites: account.NewInviteService(repo, adminID, account.RandomInviteCode),
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "list":
		return a.list(rest)
	case "approve":
		return a.eachID(rest, "批准", a.review.Approve)
	case "reject":
		return a.eachID(rest, "拒绝", a.review.Reject)
	case "invite":
		return a.invite(rest)
	default:
		return fmt.Errorf("未知命令 %q，用 im-admin help 查看用法", cmd)
	}
}

func osUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// ---------- 审核 ----------

func (a *app) list(args []string) error {
	status := account.StatusPending
	if len(args) > 0 {
		status = account.AccountStatus(args[0])
	}
	if status != account.StatusPending && status != account.StatusRejected {
		return fmt.Errorf("只能列出 pending 或 rejected，got %q", status)
	}
	items, err := a.review.List(a.ctx, status)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Fprintf(a.out, "没有 %s 的账号\n", status)
		return nil
	}

	w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "用户ID\t用户名\t注册时间\t邀请码ID\t邀请码\t备注\t邀请人ID")
	for _, it := range items {
		invID, code, note, by := "-", "-", "", "-"
		if inv := it.Invite; inv != nil {
			invID, code, note, by = strconv.FormatInt(inv.ID, 10), inv.Code, inv.Note, strconv.FormatInt(inv.CreatedBy, 10)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", it.User.ID, it.User.Username, fmtTime(it.User.CreatedAt), invID, code, note, by)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "共 %d 个", len(items))
	if len(items) >= a.review.ListLimit() {
		fmt.Fprintf(a.out, "（只显示最早的 %d 个，处理后再查看剩下的）", a.review.ListLimit())
	}
	fmt.Fprintln(a.out)
	return nil
}

// eachID 对每个用户 ID 执行审核操作。一个失败不影响其他的，最后汇总
func (a *app) eachID(args []string, action string, op func(context.Context, int64) error) error {
	if len(args) == 0 {
		return fmt.Errorf("请提供要%s的用户 ID", action)
	}
	failed := 0
	for _, arg := range args {
		id, ok := account.ParseID(arg)
		if !ok {
			fmt.Fprintf(a.out, "✗ %s：不是合法的 10 位用户 ID\n", arg)
			failed++
			continue
		}
		switch err := op(a.ctx, id); {
		case err == nil:
			fmt.Fprintf(a.out, "✓ 已%s %d\n", action, id)
		case errors.Is(err, account.ErrUserNotFound):
			fmt.Fprintf(a.out, "✗ %d：账号不存在\n", id)
			failed++
		case errors.Is(err, account.ErrInvalidStatusTransition):
			fmt.Fprintf(a.out, "✗ %d：当前状态不能%s（%v）\n", id, action, err)
			failed++
		default:
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d 个账号未能%s", failed, action)
	}
	return nil
}

// ---------- 邀请码 ----------

func (a *app) invite(args []string) error {
	if len(args) == 0 {
		return errors.New("缺少子命令：create | list | show | revoke")
	}
	if err := a.invites.VerifyAdmin(a.ctx); err != nil {
		return fmt.Errorf("ADMIN_USER_ID 配置有误（管理员账号必须存在并且已通过审核）：%w", err)
	}
	adminID := a.invites.AdminID()

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "create":
		fs := flag.NewFlagSet("invite create", flag.ContinueOnError)
		fs.SetOutput(a.out)
		expires := fs.Duration("expires", account.DefaultInviteTTL, "有效期，如 24h、72h，1 小时～30 天")
		note := fs.String("note", "", "备注，如\"给小王\"，最多 50 个字符")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		// InviteOptions 的零值表示"用默认值"（网页生成时就是这样），命令行里显式写 0 要当成错误，不能悄悄换成默认值
		if *expires <= 0 {
			return errors.New("-expires 必须大于 0")
		}
		c, err := a.invites.Create(a.ctx, adminID, account.InviteOptions{Note: *note, TTL: *expires})
		switch {
		case errors.Is(err, account.ErrInvalidInviteNote):
			return errors.New("备注最多 50 个字符")
		case errors.Is(err, account.ErrInvalidInviteOptions):
			return errors.New("有效期需在 1 小时～30 天之间")
		case err != nil:
			return err
		}
		fmt.Fprintf(a.out, "邀请码：%s（ID %d），只能提交一次注册申请，%s 过期\n", c.Code, c.ID, fmtTime(c.ExpiresAt))
		if base := strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/"); base != "" {
			fmt.Fprintf(a.out, "邀请链接：%s/register#invite=%s\n", base, c.Code)
		}
		return nil

	case "list":
		list, err := a.invites.List(a.ctx, adminID)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintln(a.out, "还没有生成过邀请码")
			return nil
		}
		w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\t邀请码\t状态\t过期时间\t生成时间\t使用者\t验证次数\t备注")
		now := time.Now()
		for _, d := range list {
			usedBy := "-"
			if d.UsedBy != nil {
				usedBy = fmt.Sprintf("%s(%d)", d.UsedBy.Username, d.UsedBy.ID)
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
				d.ID, d.Code, stateText(d.StateAt(now)), fmtTime(d.ExpiresAt), fmtTime(d.CreatedAt), usedBy, len(d.Verifications), d.Note)
		}
		return w.Flush()

	case "show", "revoke":
		if len(rest) != 1 {
			return fmt.Errorf("用法：im-admin invite %s <邀请码ID>", cmd)
		}
		id, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("%q 不是合法的邀请码 ID", rest[0])
		}
		var d *account.InviteDetail
		if cmd == "show" {
			d, err = a.invites.Get(a.ctx, adminID, id)
		} else {
			d, err = a.invites.Revoke(a.ctx, adminID, id)
		}
		if errors.Is(err, account.ErrInviteNotFound) {
			return fmt.Errorf("邀请码 %d 不存在", id)
		}
		if err != nil {
			return err
		}
		return a.printInvite(d)

	default:
		return fmt.Errorf("未知子命令 invite %s", cmd)
	}
}

func (a *app) printInvite(d *account.InviteDetail) error {
	now := time.Now()
	fmt.Fprintf(a.out, "邀请码 %s（ID %d）：%s\n", d.Code, d.ID, stateText(d.StateAt(now)))
	fmt.Fprintf(a.out, "生成者 %d，生成于 %s，%s 过期\n", d.CreatedBy, fmtTime(d.CreatedAt), fmtTime(d.ExpiresAt))
	if d.StateAt(now) == account.InviteReserved {
		fmt.Fprintf(a.out, "冻结中（有人正在填写注册表单），冻结到 %s\n", fmtTime(*d.ReservedUntil))
	}
	if d.RevokedAt != nil {
		fmt.Fprintf(a.out, "作废于 %s\n", fmtTime(*d.RevokedAt))
	}
	if d.Note != "" {
		fmt.Fprintf(a.out, "备注：%s\n", d.Note)
	}
	if u := d.UsedBy; u != nil {
		fmt.Fprintf(a.out, "使用者：%s（ID %d），%s，提交于 %s\n", u.Username, u.ID, u.Status, fmtTime(u.CreatedAt))
	} else {
		fmt.Fprintln(a.out, "还没有人用它提交注册申请")
	}
	if len(d.Verifications) == 0 {
		fmt.Fprintln(a.out, "没有验证记录")
		return nil
	}
	fmt.Fprintln(a.out, "验证记录（最新的在前）：")
	w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  时间\tIP\t结果")
	for _, v := range d.Verifications {
		fmt.Fprintf(w, "  %s\t%s\t%s\n", fmtTime(v.CreatedAt), v.IP, verifyResultText(v.Result))
	}
	return w.Flush()
}

func stateText(s account.InviteState) string {
	switch s {
	case account.InviteActive:
		return "可用"
	case account.InviteReserved:
		return "冻结中"
	case account.InviteUsed:
		return "已使用"
	case account.InviteExpired:
		return "已过期"
	case account.InviteRevoked:
		return "已作废"
	}
	return string(s)
}

func verifyResultText(r account.VerifyResult) string {
	switch r {
	case account.VerifyReserved:
		return "验证成功，冻结"
	case account.VerifyInvalid:
		return "无效（已过期 / 已使用 / 已作废）"
	case account.VerifyInUse:
		return "被拒绝：正被别人使用"
	case account.VerifyLimited:
		return "被拒绝：验证太频繁"
	}
	return string(r)
}

// fmtTime 按服务器本地时区显示，带上时区缩写，避免把 UTC 时间误读成北京时间
func fmtTime(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04 MST")
}
