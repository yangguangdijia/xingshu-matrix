package bastion

import (
	"bufio"
	"context"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/ssh"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// MaskRule 是一条终端输出脱敏规则（JumpServer「数据脱敏」的终端场景）。
type MaskRule struct {
	ID          int64     `db:"id" json:"id"`
	Name        string    `db:"name" json:"name"`
	Pattern     string    `db:"pattern" json:"pattern"`
	Replacement string    `db:"replacement" json:"replacement"`
	Enabled     bool      `db:"enabled" json:"enabled"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
}

type maskIn struct {
	Name        string `json:"name" binding:"required,max=64"`
	Pattern     string `json:"pattern" binding:"required,max=512"`
	Replacement string `json:"replacement"`
	Enabled     *bool  `json:"enabled"`
}

func (in *maskIn) check() error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return httpx.Validation("name is required")
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return httpx.Validation("invalid regex: " + err.Error())
	}
	if re.MatchString("") {
		return httpx.Validation("pattern must not match an empty string")
	}
	if in.Replacement == "" {
		in.Replacement = "******"
	}
	if len(in.Replacement) > 64 {
		return httpx.Validation("replacement too long")
	}
	return nil
}

func (m *Module) listMasks(c *gin.Context) {
	items, err := queryAll[MaskRule](c, m, `SELECT * FROM bastion_mask_rules ORDER BY id`)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	ok(c, items)
}

func (m *Module) createMask(c *gin.Context) {
	var in maskIn
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.check(); err != nil {
		httpx.Fail(c, err)
		return
	}
	r, err := queryOne[MaskRule](c, m, `INSERT INTO bastion_mask_rules (name,pattern,replacement,enabled) VALUES ($1,$2,$3,$4) RETURNING *`,
		in.Name, in.Pattern, in.Replacement, in.Enabled == nil || *in.Enabled)
	if err != nil {
		httpx.Fail(c, mapPgErr(err))
		return
	}
	audit.SetResource(c, "bastion_mask_rule", itoa(r.ID))
	httpx.Created(c, r)
}

func (m *Module) updateMask(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	var in maskIn
	if !httpx.Bind(c, &in) {
		return
	}
	if err := in.check(); err != nil {
		httpx.Fail(c, err)
		return
	}
	r, err := queryOne[MaskRule](c, m, `UPDATE bastion_mask_rules SET name=$2,pattern=$3,replacement=$4,enabled=$5 WHERE id=$1 RETURNING *`,
		id, in.Name, in.Pattern, in.Replacement, in.Enabled == nil || *in.Enabled)
	if err != nil {
		httpx.Fail(c, mapPgErr(err))
		return
	}
	audit.SetResource(c, "bastion_mask_rule", itoa(id))
	ok(c, r)
}

func (m *Module) deleteMask(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if okk && m.exec(c, `DELETE FROM bastion_mask_rules WHERE id=$1`, id) {
		audit.SetResource(c, "bastion_mask_rule", itoa(id))
		ok(c, gin.H{"id": id})
	}
}

// previewMask 用当前已启用规则（或请求里给出的单条规则）处理一段示例文本，便于保存前验证。
func (m *Module) previewMask(c *gin.Context) {
	var in struct {
		Sample      string `json:"sample" binding:"required,max=8192"`
		Pattern     string `json:"pattern"`
		Replacement string `json:"replacement"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	var mk *Masker
	if in.Pattern != "" {
		r := maskIn{Name: "preview", Pattern: in.Pattern, Replacement: in.Replacement}
		if err := r.check(); err != nil {
			httpx.Fail(c, err)
			return
		}
		mk = &Masker{rules: []compiledMask{{re: regexp.MustCompile(r.Pattern), repl: r.Replacement}}}
	} else {
		var err error
		if mk, err = m.loadMasker(c); err != nil {
			httpx.Fail(c, err)
			return
		}
	}
	ok(c, gin.H{"output": string(mk.Apply([]byte(in.Sample)))})
}

type compiledMask struct {
	re   *regexp.Regexp
	repl string
}

// Masker 对终端输出逐块做正则替换。匹配在单个输出块内进行：
// 被拆到两个网络块之间的敏感串可能漏掉，这是不缓冲输出（不拖慢交互回显）的代价，文档中已说明。
type Masker struct{ rules []compiledMask }

// Apply 返回脱敏后的内容；没有规则时原样返回。
func (k *Masker) Apply(b []byte) []byte {
	if k == nil || len(k.rules) == 0 {
		return b
	}
	for _, r := range k.rules {
		b = r.re.ReplaceAllLiteral(b, []byte(r.repl))
	}
	return b
}

func (m *Module) loadMasker(ctx context.Context) (*Masker, error) {
	rs, err := queryAll[MaskRule](ctx, m, `SELECT * FROM bastion_mask_rules WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	k := &Masker{}
	for _, r := range rs {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, httpx.Conflict("an enabled mask rule is invalid; connections are disabled")
		}
		k.rules = append(k.rules, compiledMask{re: re, repl: r.Replacement})
	}
	return k, nil
}

// testAsset 做真实连通性探测：TCP 建连 → 读取协议横幅；SSH 资产给出 account_id 时再做一次登录验证（不开 Shell）。
func (m *Module) testAsset(c *gin.Context) {
	id, okk := httpx.ParamID(c, "id")
	if !okk {
		return
	}
	var in struct {
		AccountID int64 `json:"account_id"`
	}
	_ = c.ShouldBindJSON(&in)
	asset, err := queryOne[Asset](c, m, `SELECT * FROM bastion_assets WHERE id=$1`, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if p := rbac.PrincipalFrom(c); p != nil && !p.CanEnv(asset.Env) {
		httpx.Fail(c, httpx.Forbidden("environment "+asset.Env+" not in your scope"))
		return
	}
	res := gin.H{"asset_id": asset.ID, "address": net.JoinHostPort(asset.Host, strconv.Itoa(asset.Port))}
	start := time.Now()
	ctx, cancel := context.WithTimeout(c, 6*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", res["address"].(string))
	if err != nil {
		res["reachable"] = false
		res["error"] = dialReason(err)
		ok(c, res)
		return
	}
	res["reachable"] = true
	res["latency_ms"] = time.Since(start).Milliseconds()
	if asset.Protocol == "ssh" {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		line, _ := bufio.NewReader(conn).ReadString('\n')
		res["banner"] = strings.TrimSpace(line)
	}
	_ = conn.Close()
	if asset.Protocol == "ssh" && in.AccountID > 0 {
		acc, err := queryOne[Account](c, m, `SELECT * FROM bastion_accounts WHERE id=$1 AND asset_id=$2`, in.AccountID, asset.ID)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		auth, err := m.authMethod(acc)
		if err != nil {
			res["auth"] = false
			res["error"] = "托管凭据无法解密或格式错误"
			ok(c, res)
			return
		}
		cl, err := ssh.Dial("tcp", res["address"].(string), &ssh.ClientConfig{
			User: acc.Username, Auth: []ssh.AuthMethod{auth},
			HostKeyCallback: m.hostKeyCallback(asset), Timeout: 8 * time.Second,
		})
		if err != nil {
			res["auth"] = false
			if strings.Contains(err.Error(), "host key mismatch") {
				res["error"] = "主机指纹与登记的不一致，可能被替换或重装"
			} else {
				res["error"] = "登录失败：账号或凭据不正确"
			}
			ok(c, res)
			return
		}
		_ = cl.Close()
		res["auth"] = true
	}
	ok(c, res)
}

func dialReason(err error) string {
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		return "连接超时：地址不可达或被防火墙拦截"
	case strings.Contains(err.Error(), "refused"):
		return "端口拒绝连接：服务未启动或端口错误"
	case strings.Contains(err.Error(), "no such host"):
		return "域名无法解析"
	default:
		return "网络连接失败"
	}
}
