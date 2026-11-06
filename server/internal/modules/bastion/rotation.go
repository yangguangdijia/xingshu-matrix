package bastion

// 改密计划：为托管的「密码」类 SSH 账号定期生成强随机密码并在主机上修改。
//
// 单个账号的流程（rotateAccount）：
//  1. 生成新密码，先加密写入 pending_secret_enc（防止改了主机却丢了新密码）；
//  2. 用当前密码登录：root 用 chpasswd，普通用户在伪终端里运行 passwd 并按提示输入旧/新密码；
//  3. 用新密码重新登录验证；成功则把 pending 转为正式凭据并记录 password_changed_at；
//  4. 失败时判断主机上的实际密码：改密前就登录失败 → 主机未改，丢弃 pending；
//     旧密码仍可用 → 丢弃 pending；旧密码不可用但新密码可用 → 视为成功并保存；
//     新旧都不可用 → 主机很可能已改，保留新密码为正式凭据并标记失败，提示人工核对。
// 判断逻辑在 rotation.Rotate 中，通过 rotationHost / rotationStore 接口注入，单元测试用假主机覆盖各分支。
// 私钥账号会被跳过。计划按 interval_days 由后台每分钟检查一次（PostgreSQL advisory lock 保证多副本只跑一份）。

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/ssh"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const (
	pwLower  = "abcdefghijkmnopqrstuvwxyz"
	pwUpper  = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	pwDigits = "23456789"
	pwSymbol = "!@#%^*-_=+"
)

// GeneratePassword 生成包含大小写字母、数字与符号的随机密码。
func GeneratePassword(n int) (string, error) {
	if n < 12 {
		n = 12
	}
	sets := []string{pwLower, pwUpper, pwDigits, pwSymbol}
	all := strings.Join(sets, "")
	out := make([]byte, 0, n)
	pick := func(set string) error {
		i, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		if err != nil {
			return err
		}
		out = append(out, set[i.Int64()])
		return nil
	}
	for _, s := range sets {
		if err := pick(s); err != nil {
			return "", err
		}
	}
	for len(out) < n {
		if err := pick(all); err != nil {
			return "", err
		}
	}
	for i := len(out) - 1; i > 0; i-- { // Fisher-Yates
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return "", err
		}
		out[i], out[j.Int64()] = out[j.Int64()], out[i]
	}
	return string(out), nil
}

// passwdAnswer 根据 passwd 的提示返回应输入的内容（"" 表示不是密码提示）。
func passwdAnswer(prompt, oldPw, newPw string) string {
	p := strings.ToLower(prompt)
	if !strings.Contains(p, "password") && !strings.Contains(p, "密码") {
		return ""
	}
	switch {
	case strings.Contains(p, "current") || strings.Contains(p, "old") || strings.Contains(p, "当前") || strings.Contains(p, "旧"):
		return oldPw
	case strings.Contains(p, "new") || strings.Contains(p, "retype") || strings.Contains(p, "新") || strings.Contains(p, "again"):
		return newPw
	}
	return oldPw // 部分系统第一个提示是 "Password:"
}

type rotationTarget struct {
	acc   *Account
	asset *Asset
}

func (m *Module) loadTarget(ctx context.Context, accountID int64) (*rotationTarget, error) {
	acc, err := queryOne[Account](ctx, m, `SELECT * FROM bastion_accounts WHERE id=$1`, accountID)
	if err != nil {
		return nil, err
	}
	asset, err := queryOne[Asset](ctx, m, `SELECT * FROM bastion_assets WHERE id=$1`, acc.AssetID)
	if err != nil {
		return nil, err
	}
	return &rotationTarget{acc: acc, asset: asset}, nil
}

func (m *Module) dialPassword(t *rotationTarget, pw string) (*ssh.Client, error) {
	addr := net.JoinHostPort(t.asset.Host, strconv.Itoa(t.asset.Port))
	return ssh.Dial("tcp", addr, &ssh.ClientConfig{User: t.acc.Username, Auth: []ssh.AuthMethod{ssh.Password(pw)},
		HostKeyCallback: m.hostKeyCallback(t.asset), Timeout: 10 * time.Second})
}

// changeOnHost 在主机上把密码从 oldPw 改为 newPw。
func (m *Module) changeOnHost(t *rotationTarget, oldPw, newPw string) error {
	cl, err := m.dialPassword(t, oldPw)
	if err != nil {
		return fmt.Errorf("%w: %v", errOldLogin, err)
	}
	defer cl.Close()
	sess, err := cl.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	if t.acc.Username == "root" {
		sess.Stdin = strings.NewReader("root:" + newPw + "\n")
		var out bytes.Buffer
		sess.Stdout, sess.Stderr = &out, &out
		if err := sess.Run("chpasswd"); err != nil {
			return fmt.Errorf("chpasswd 失败: %v %s", err, strings.TrimSpace(out.String()))
		}
		return nil
	}
	if err := sess.RequestPty("xterm", 24, 80, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
		return err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return err
	}
	if err := sess.Start("LANG=C passwd"); err != nil {
		return err
	}
	var all, line bytes.Buffer
	buf := make([]byte, 512)
	answered := 0
	readDone := make(chan error, 1)
	go func() {
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				all.Write(buf[:n])
				line.Write(buf[:n])
				txt := strings.TrimRight(line.String(), " \t")
				if strings.HasSuffix(txt, ":") {
					if ans := passwdAnswer(txt[strings.LastIndexAny(txt, "\r\n")+1:], oldPw, newPw); ans != "" && answered < 4 {
						_, _ = stdin.Write([]byte(ans + "\n"))
						answered++
					}
					line.Reset()
				}
				if i := bytes.LastIndexAny(line.Bytes(), "\r\n"); i >= 0 {
					rest := append([]byte(nil), line.Bytes()[i+1:]...)
					line.Reset()
					line.Write(rest)
				}
			}
			if err != nil {
				readDone <- err
				return
			}
		}
	}()
	waitErr := make(chan error, 1)
	go func() { waitErr <- sess.Wait() }()
	select {
	case err := <-waitErr:
		<-time.After(100 * time.Millisecond)
		out := strings.TrimSpace(strings.ReplaceAll(all.String(), "\r", ""))
		if err != nil {
			return fmt.Errorf("passwd 失败: %s", lastLine(out))
		}
		return nil
	case <-time.After(30 * time.Second):
		return errors.New("passwd 超时（可能出现了无法识别的提示）")
	}
}

func lastLine(s string) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(ls[len(ls)-1])
}

// errOldLogin 表示改密前用当前密码就登录不上：主机一定没有被修改。
var errOldLogin = errors.New("用当前密码登录失败")

// rotationHost 是改密流程对目标主机的操作（生产实现走 SSH，测试用假实现）。
type rotationHost interface {
	// Change 用 oldPw 登录并把密码改为 newPw。
	Change(t *rotationTarget, oldPw, newPw string) error
	// Login 只验证 pw 能否登录（不打开 Shell）。
	Login(t *rotationTarget, pw string) error
}

// rotationStore 是改密流程对账号凭据的持久化操作。
type rotationStore interface {
	SavePending(ctx context.Context, accountID int64, enc string) error
	// Commit 把待生效密码转为正式凭据并记录改密时间。
	Commit(ctx context.Context, accountID int64) error
	// Discard 丢弃待生效密码（主机未改）。
	Discard(ctx context.Context, accountID int64) error
}

type secretBox interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(envelope string) (string, error)
}

// rotation 执行单个账号的改密，依赖全部通过接口注入，便于测试成功与失败回滚判断。
type rotation struct {
	host  rotationHost
	store rotationStore
	box   secretBox
	gen   func(int) (string, error)
}

// Rotate 轮换 t 的密码，返回 succeeded / failed / skipped 与说明。
func (r rotation) Rotate(ctx context.Context, t *rotationTarget, length int) (status, message string) {
	if t.asset.Protocol != "ssh" {
		return "skipped", "只支持 SSH 资产"
	}
	if t.acc.AuthType != "password" {
		return "skipped", "私钥账号不做改密"
	}
	oldPw, err := r.box.Decrypt(t.acc.Secret)
	if err != nil {
		return "failed", "当前凭据无法解密"
	}
	gen := r.gen
	if gen == nil {
		gen = GeneratePassword
	}
	newPw, err := gen(length)
	if err != nil {
		return "failed", "生成密码失败"
	}
	enc, err := r.box.Encrypt(newPw)
	if err != nil {
		return "failed", "加密新密码失败"
	}
	// 先保存待生效密码，保证主机改了之后新密码不会丢
	if err := r.store.SavePending(ctx, t.acc.ID, enc); err != nil {
		return "failed", "保存待生效密码失败，未改动主机"
	}
	changeErr := r.host.Change(t, oldPw, newPw)
	if changeErr == nil && r.host.Login(t, newPw) == nil {
		if err := r.store.Commit(ctx, t.acc.ID); err != nil {
			return "failed", "主机已改密但保存失败，新密码保留在待生效凭据中，请勿重复执行并联系管理员"
		}
		return "succeeded", "已改密并用新密码验证登录"
	}
	// 失败：判断主机上的密码到底是哪一个
	if errors.Is(changeErr, errOldLogin) {
		// 还没登录上就失败了，主机未改：丢弃新密码，保留原凭据
		_ = r.store.Discard(ctx, t.acc.ID)
		return "failed", "用当前密码登录失败（主机不可达或密码已在别处被修改），未改密"
	}
	if r.host.Login(t, oldPw) == nil {
		_ = r.store.Discard(ctx, t.acc.ID)
		msg := "改密未生效，原密码仍可用"
		if changeErr != nil {
			msg += "：" + changeErr.Error()
		} else {
			msg += "：新密码验证登录失败"
		}
		return "failed", msg
	}
	if changeErr != nil && r.host.Login(t, newPw) == nil {
		// 改密命令报错（例如输出异常、连接中断），但新密码实际已生效
		if err := r.store.Commit(ctx, t.acc.ID); err != nil {
			return "failed", "主机已改密但保存失败，新密码保留在待生效凭据中，请勿重复执行并联系管理员"
		}
		return "succeeded", "改密过程有报错，但新密码已生效并验证登录"
	}
	// 新旧密码都登录不上：主机很可能已经改了，保留新密码作为正式凭据，提示人工核对
	_ = r.store.Commit(ctx, t.acc.ID)
	return "failed", "无法用新旧密码确认登录，已保存新密码，请人工核对主机账号"
}

// sshRotationHost 是 rotationHost 的 SSH 实现。
type sshRotationHost struct{ m *Module }

func (h sshRotationHost) Change(t *rotationTarget, oldPw, newPw string) error {
	return h.m.changeOnHost(t, oldPw, newPw)
}

func (h sshRotationHost) Login(t *rotationTarget, pw string) error {
	cl, err := h.m.dialPassword(t, pw)
	if err != nil {
		return err
	}
	return cl.Close()
}

// pgRotationStore 是 rotationStore 的 PostgreSQL 实现。
type pgRotationStore struct{ m *Module }

func (s pgRotationStore) SavePending(ctx context.Context, id int64, enc string) error {
	_, err := s.m.d.DB.Exec(ctx, `UPDATE bastion_accounts SET pending_secret_enc=$2 WHERE id=$1`, id, enc)
	return err
}

func (s pgRotationStore) Commit(ctx context.Context, id int64) error {
	tag, err := s.m.d.DB.Exec(ctx, `UPDATE bastion_accounts SET secret_enc=pending_secret_enc, pending_secret_enc='', password_changed_at=now(), updated_at=now()
		WHERE id=$1 AND pending_secret_enc<>''`, id)
	if err == nil && tag.RowsAffected() != 1 {
		return errors.New("pending secret missing")
	}
	return err
}

func (s pgRotationStore) Discard(ctx context.Context, id int64) error {
	_, err := s.m.d.DB.Exec(ctx, `UPDATE bastion_accounts SET pending_secret_enc='' WHERE id=$1`, id)
	return err
}

// rotateAccount 轮换一个账号的密码，返回结果状态与说明。
func (m *Module) rotateAccount(ctx context.Context, accountID int64, length int) (status, message string) {
	t, err := m.loadTarget(ctx, accountID)
	if err != nil {
		return "failed", "账号不存在"
	}
	return rotation{host: sshRotationHost{m}, store: pgRotationStore{m}, box: m.d.Crypto}.Rotate(ctx, t, length)
}

func (m *Module) recordRun(ctx context.Context, planID any, accountID int64, by, status, msg string, started time.Time) {
	var asset, user string
	_ = m.d.DB.QueryRow(ctx, `SELECT a.name, acc.username FROM bastion_accounts acc JOIN bastion_assets a ON a.id=acc.asset_id WHERE acc.id=$1`, accountID).Scan(&asset, &user)
	_, _ = m.d.DB.Exec(ctx, `INSERT INTO bastion_rotation_runs (plan_id,account_id,asset,username,status,message,triggered_by,started_at,finished_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now())`, planID, accountID, asset, user, status, msg, by, started)
}

// runPlan 执行一个计划的全部账号，更新下次执行时间与结果摘要。
func (m *Module) runPlan(ctx context.Context, planID int64, by string) (int, int, error) {
	var ids []int64
	var length, interval int
	if err := m.d.DB.QueryRow(ctx, `SELECT account_ids, length, interval_days FROM bastion_rotation_plans WHERE id=$1`, planID).Scan(&ids, &length, &interval); err != nil {
		return 0, 0, err
	}
	ok, failed := 0, 0
	for _, id := range ids {
		started := time.Now()
		st, msg := m.rotateAccount(ctx, id, length)
		m.recordRun(ctx, planID, id, by, st, msg, started)
		switch st {
		case "succeeded":
			ok++
		case "failed":
			failed++
		}
	}
	summary := fmt.Sprintf("成功 %d，失败 %d，共 %d 个账号", ok, failed, len(ids))
	_, err := m.d.DB.Exec(ctx, `UPDATE bastion_rotation_plans SET last_run_at=now(), last_result=$2,
		next_run_at=now() + make_interval(days => interval_days), updated_at=now() WHERE id=$1`, planID, summary)
	return ok, failed, err
}

// rotationLoop 每分钟检查到期的计划。
func (m *Module) rotationLoop(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.runDuePlans(ctx)
		}
	}
}

func (m *Module) runDuePlans(ctx context.Context) {
	conn, err := m.d.DB.Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()
	var leader bool
	if conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(82178066)").Scan(&leader) != nil || !leader {
		return
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock(82178066)") //nolint:errcheck
	rows, err := m.d.DB.Query(ctx, `SELECT id FROM bastion_rotation_plans WHERE enabled AND next_run_at <= now() ORDER BY next_run_at LIMIT 10`)
	if err != nil {
		return
	}
	var due []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			due = append(due, id)
		}
	}
	rows.Close()
	for _, id := range due {
		if _, _, err := m.runPlan(ctx, id, "计划任务"); err != nil && m.d.Logger != nil {
			m.d.Logger.Warn("bastion: rotation plan failed", "plan", id, "err", err)
		}
	}
}

// ---------- HTTP ----------

type planIn struct {
	Name         string     `json:"name" binding:"required,max=100"`
	AccountIDs   []int64    `json:"account_ids" binding:"required,min=1"`
	IntervalDays int        `json:"interval_days" binding:"required,min=1,max=365"`
	Length       int        `json:"length"`
	Enabled      *bool      `json:"enabled"`
	NextRunAt    *time.Time `json:"next_run_at"`
}

func (m *Module) registerRotation(g *gin.RouterGroup) {
	g.GET("/rotation-plans", rbac.RequirePerm(PermRead), m.listPlans)
	g.POST("/rotation-plans", rbac.RequirePerm(PermManage), m.savePlan)
	g.PUT("/rotation-plans/:id", rbac.RequirePerm(PermManage), m.savePlan)
	g.DELETE("/rotation-plans/:id", rbac.RequirePerm(PermManage), m.deletePlan)
	g.POST("/rotation-plans/:id/run", rbac.RequirePerm(PermManage), m.runPlanNow)
	g.GET("/rotation-runs", rbac.RequirePerm(PermRead), m.listRuns)
	g.POST("/accounts/:id/rotate", rbac.RequirePerm(PermManage), m.rotateNow)
}

func rowsToMaps(c *gin.Context, m *Module, sql string, args ...any) ([]map[string]any, error) {
	rows, err := m.d.DB.Query(c, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		r := map[string]any{}
		for i, f := range rows.FieldDescriptions() {
			r[f.Name] = vals[i]
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (m *Module) listPlans(c *gin.Context) {
	items, err := rowsToMaps(c, m, `SELECT * FROM bastion_rotation_plans ORDER BY id`)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	ok(c, gin.H{"items": items})
}

func (m *Module) savePlan(c *gin.Context) {
	var in planIn
	if !httpx.Bind(c, &in) {
		return
	}
	if in.Length == 0 {
		in.Length = 20
	}
	if in.Length < 12 || in.Length > 64 {
		httpx.Fail(c, httpx.Validation("length must be 12-64"))
		return
	}
	var n int
	if err := m.d.DB.QueryRow(c, `SELECT count(*) FROM bastion_accounts WHERE id = ANY($1)`, in.AccountIDs).Scan(&n); err != nil || n != len(in.AccountIDs) {
		httpx.Fail(c, httpx.Validation("account_ids contains unknown accounts"))
		return
	}
	en := in.Enabled == nil || *in.Enabled
	next := time.Now().Add(time.Duration(in.IntervalDays) * 24 * time.Hour)
	if in.NextRunAt != nil {
		next = *in.NextRunAt
	}
	var id int64
	var err error
	if c.Param("id") == "" {
		err = m.d.DB.QueryRow(c, `INSERT INTO bastion_rotation_plans (name,account_ids,interval_days,length,enabled,next_run_at,created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`, strings.TrimSpace(in.Name), in.AccountIDs, in.IntervalDays, in.Length, en, next, actorName(c)).Scan(&id)
	} else {
		pid, okk := httpx.ParamID(c, "id")
		if !okk {
			return
		}
		err = m.d.DB.QueryRow(c, `UPDATE bastion_rotation_plans SET name=$2,account_ids=$3,interval_days=$4,length=$5,enabled=$6,
			next_run_at=COALESCE($7,next_run_at),updated_at=now() WHERE id=$1 RETURNING id`, pid, strings.TrimSpace(in.Name), in.AccountIDs, in.IntervalDays, in.Length, en, in.NextRunAt).Scan(&id)
	}
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			httpx.Fail(c, httpx.Conflict("计划名称已存在"))
			return
		}
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "bastion_rotation_plan", strconv.FormatInt(id, 10))
	items, _ := rowsToMaps(c, m, `SELECT * FROM bastion_rotation_plans WHERE id=$1`, id)
	if len(items) == 1 {
		ok(c, items[0])
		return
	}
	ok(c, gin.H{"id": id})
}

func (m *Module) deletePlan(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	tag, err := m.d.DB.Exec(c, `DELETE FROM bastion_rotation_plans WHERE id=$1`, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Fail(c, httpx.NotFound("not found"))
		return
	}
	audit.SetResource(c, "bastion_rotation_plan", strconv.FormatInt(id, 10))
	ok(c, gin.H{"id": id})
}

func (m *Module) runPlanNow(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	ctx, cancel := context.WithTimeout(c, 5*time.Minute)
	defer cancel()
	s, f, err := m.runPlan(ctx, id, actorName(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "bastion_rotation_plan", strconv.FormatInt(id, 10))
	ok(c, gin.H{"succeeded": s, "failed": f})
}

func (m *Module) rotateNow(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	var in struct {
		Length int `json:"length"`
	}
	_ = c.ShouldBindJSON(&in)
	if in.Length == 0 {
		in.Length = 20
	}
	if in.Length < 12 || in.Length > 64 {
		httpx.Fail(c, httpx.Validation("length must be 12-64"))
		return
	}
	if t, err := m.loadTarget(c, id); err != nil {
		httpx.Fail(c, err)
		return
	} else if p := rbac.PrincipalFrom(c); p != nil && !p.CanEnv(t.asset.Env) {
		httpx.Fail(c, httpx.Forbidden("environment "+t.asset.Env+" not in your scope"))
		return
	}
	ctx, cancel := context.WithTimeout(c, 2*time.Minute)
	defer cancel()
	started := time.Now()
	st, msg := m.rotateAccount(ctx, id, in.Length)
	m.recordRun(ctx, nil, id, actorName(c), st, msg, started)
	audit.SetResource(c, "bastion_account", strconv.FormatInt(id, 10))
	ok(c, gin.H{"status": st, "message": msg})
}

func (m *Module) listRuns(c *gin.Context) {
	q := `SELECT r.*, COALESCE(p.name,'手动') AS plan_name FROM bastion_rotation_runs r LEFT JOIN bastion_rotation_plans p ON p.id=r.plan_id`
	var args []any
	if v := c.Query("plan_id"); v != "" {
		pid, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			httpx.Fail(c, httpx.Validation("plan_id"))
			return
		}
		q += ` WHERE r.plan_id=$1`
		args = append(args, pid)
	}
	q += ` ORDER BY r.id DESC LIMIT 100`
	items, err := rowsToMaps(c, m, q, args...)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	ok(c, gin.H{"items": items})
}

func actorName(c *gin.Context) string {
	if p := rbac.PrincipalFrom(c); p != nil {
		return p.Username
	}
	return ""
}
