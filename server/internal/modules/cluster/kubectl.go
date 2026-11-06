package cluster

// kubectl.go：顶部栏「kubectl」Web 终端的后端。
//
// 平台不在容器里跑 kubectl 二进制，而是在服务端解析常用 kubectl 子命令，直接用
// 该集群的 client-go 客户端执行（直连 kubeconfig 与 Agent 隧道两种接入方式都适用）：
//   - 只读命令（get / describe / logs / top / api-resources / version / cluster-info /
//     rollout status|history）需要 workload:read，输出与 kubectl 一致（get 使用服务端 Table）。
//   - 变更命令（scale / rollout restart / delete / cordon / uncordon）不直接写集群，而是转成
//     平台的变更单，走与界面操作相同的环境审批、权限与审计（runChange）。
//   - exec 需要 workload:write，以非交互方式在容器内执行单条命令（30 秒超时），并记审计。
// 不支持管道、重定向和多条命令串联；apply -f 请使用「YAML 编辑」或「创建资源」。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/yaml"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const (
	kubectlMaxOutput = 1 << 20 // 单条命令最多返回 1 MiB 输出
	kubectlTimeout   = 30 * time.Second
	kubectlMaxLine   = 4096
)

type kubectlRequest struct {
	Command   string `json:"command" binding:"required"`
	Namespace string `json:"namespace"`
	Reason    string `json:"reason"`
}

type kubectlResponse struct {
	Output    string `json:"output"`
	ExitCode  int    `json:"exit_code"`
	Namespace string `json:"namespace"`
	Truncated bool   `json:"truncated,omitempty"`
	// Ticket 非空表示变更命令已提交审批，尚未执行。
	Ticket *approval.Ticket `json:"ticket,omitempty"`
	// Change 为 true 表示该命令经平台变更流程执行。
	Change bool `json:"change,omitempty"`
}

// kubectlArgs 是解析后的命令行。
type kubectlArgs struct {
	verb      string
	pos       []string
	ns        string
	allNS     bool
	output    string
	selector  string
	container string
	tail      int64
	previous  bool
	replicas  *int32
	execArgs  []string
}

// splitCommandLine 按 shell 规则拆分参数（支持单双引号与反斜杠），拒绝管道、重定向与命令串联。
func splitCommandLine(line string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord, sq, dq, esc := false, false, false, false
	for _, r := range line {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\' && !sq:
			esc, inWord = true, true
		case r == '\'' && !dq:
			sq, inWord = !sq, true
		case r == '"' && !sq:
			dq, inWord = !dq, true
		case !sq && !dq && unicode.IsSpace(r):
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		case !sq && !dq && strings.ContainsRune("|;&><`$", r):
			return nil, fmt.Errorf("Web 终端不支持管道、重定向、变量和多条命令串联（%q）", r)
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if sq || dq {
		return nil, fmt.Errorf("引号未闭合")
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}

// parseKubectl 解析 kubectl 参数（首个 kubectl 可省略）。exec 的 -- 之后原样保留。
func parseKubectl(line, defNS string) (*kubectlArgs, error) {
	if len(line) > kubectlMaxLine {
		return nil, fmt.Errorf("命令过长")
	}
	words, err := splitCommandLine(strings.TrimSpace(line))
	if err != nil {
		return nil, err
	}
	if len(words) > 0 && (words[0] == "kubectl" || words[0] == "k") {
		words = words[1:]
	}
	if len(words) == 0 {
		return nil, fmt.Errorf("请输入 kubectl 子命令，输入 help 查看支持的命令")
	}
	a := &kubectlArgs{ns: defNS, tail: -1}
	for i := 0; i < len(words); i++ {
		w := words[i]
		if w == "--" {
			a.execArgs = append(a.execArgs, words[i+1:]...)
			break
		}
		if !strings.HasPrefix(w, "-") || w == "-" {
			if a.verb == "" {
				a.verb = w
			} else {
				a.pos = append(a.pos, w)
			}
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(w, "-"), "=")
		next := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(words) {
				return "", fmt.Errorf("参数 %s 缺少取值", w)
			}
			i++
			return words[i], nil
		}
		switch name {
		case "n", "namespace":
			if a.ns, err = next(); err != nil {
				return nil, err
			}
		case "A", "all-namespaces":
			a.allNS = true
		case "o", "output":
			if a.output, err = next(); err != nil {
				return nil, err
			}
		case "l", "selector":
			if a.selector, err = next(); err != nil {
				return nil, err
			}
		case "c", "container":
			if a.container, err = next(); err != nil {
				return nil, err
			}
		case "tail":
			v, e := next()
			if e != nil {
				return nil, e
			}
			n, e := strconv.ParseInt(v, 10, 64)
			if e != nil {
				return nil, fmt.Errorf("--tail 需要整数")
			}
			a.tail = n
		case "p", "previous":
			a.previous = true
		case "replicas":
			v, e := next()
			if e != nil {
				return nil, e
			}
			n, e := strconv.ParseInt(v, 10, 32)
			if e != nil || n < 0 {
				return nil, fmt.Errorf("--replicas 需要非负整数")
			}
			r := int32(n)
			a.replicas = &r
		case "i", "t", "it", "ti", "stdin", "tty", "f", "follow", "w", "watch":
			if name == "f" && (a.verb == "apply" || a.verb == "create" || a.verb == "replace" || a.verb == "delete") {
				return nil, fmt.Errorf("%s -f 不在 Web 终端中执行，请使用资源页面的「YAML 编辑」或「创建」，以便走审批与审计", a.verb)
			}
			if name == "f" || name == "follow" || name == "w" || name == "watch" {
				return nil, fmt.Errorf("Web 终端不支持持续跟踪（-f/-w），请在工作负载详情页查看实时日志")
			}
			// -it 在 Web 终端中忽略：exec 以非交互方式执行
		default:
			return nil, fmt.Errorf("不支持的参数 %s", w)
		}
	}
	if a.verb == "" {
		return nil, fmt.Errorf("请输入 kubectl 子命令")
	}
	if a.ns == "" {
		a.ns = "default"
	}
	return a, nil
}

const kubectlHelp = `星枢 Web Shell 支持的 kubectl 命令：
  只读
    get <资源>[,<资源>] [名称] [-n 命名空间|-A] [-l 选择器] [-o wide|yaml|json|name]
    describe <资源> <名称> [-n 命名空间]
    logs <Pod> [-c 容器] [--tail N] [-p]
    top nodes | top pods [-n 命名空间|-A]
    rollout status|history <类型>/<名称>
    api-resources | version | cluster-info
  变更（按集群环境走审批并记审计）
    scale <类型>/<名称> --replicas=N
    rollout restart <类型>/<名称>
    delete <类型> <名称>          （支持 Deployment/StatefulSet/DaemonSet/Job/CronJob/Pod）
    cordon | uncordon <节点>
  容器内执行（需要工作负载写权限）
    exec <Pod> [-c 容器] -- <命令> [参数...]
  其他：clear 清屏；不支持管道、重定向、-f/-w 跟踪与 apply -f。`

// kubectlExec 是 POST /clusters/:id/kubectl。
func (m *Module) kubectlExec(c *gin.Context) {
	cl, ok := m.clusterFor(c)
	if !ok {
		return
	}
	var req kubectlRequest
	if !httpx.Bind(c, &req) {
		return
	}
	p := rbac.PrincipalFrom(c)
	audit.SetAction(c, "kubectl")
	audit.SetDetail(c, map[string]any{"command": req.Command, "namespace": req.Namespace})
	a, err := parseKubectl(req.Command, req.Namespace)
	if err != nil {
		httpx.OK(c, kubectlResponse{Output: "error: " + err.Error(), ExitCode: 1, Namespace: req.Namespace})
		return
	}
	if a.verb == "help" {
		httpx.OK(c, kubectlResponse{Output: kubectlHelp, Namespace: a.ns})
		return
	}
	need := PermWorkloadRead
	switch a.verb {
	case "scale", "delete", "exec":
		need = PermWorkloadWrite
	case "rollout":
		if len(a.pos) > 0 && a.pos[0] == "restart" {
			need = PermWorkloadWrite
		}
	case "cordon", "uncordon":
		need = PermNodeWrite
	case "top":
		if len(a.pos) > 0 && strings.HasPrefix(a.pos[0], "no") {
			need = PermNodeRead
		}
	}
	if !p.Can(need) {
		httpx.OK(c, kubectlResponse{Output: "error: 当前账号没有执行该命令的权限（需要 " + need + "）", ExitCode: 1, Namespace: a.ns})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), kubectlTimeout)
	defer cancel()
	kc, err := m.kube(ctx, cl)
	if err != nil {
		httpx.OK(c, kubectlResponse{Output: "error: 无法连接集群：" + err.Error(), ExitCode: 1, Namespace: a.ns})
		return
	}
	switch a.verb {
	case "scale", "delete", "cordon", "uncordon":
		m.kubectlChange(c, p, cl, a, req.Reason)
		return
	case "rollout":
		if len(a.pos) > 0 && a.pos[0] == "restart" {
			m.kubectlChange(c, p, cl, a, req.Reason)
			return
		}
	}
	out, err := runKubectlRead(ctx, kc, a)
	resp := kubectlResponse{Output: out, Namespace: a.ns}
	if err != nil {
		if resp.Output != "" && !strings.HasSuffix(resp.Output, "\n") {
			resp.Output += "\n"
		}
		resp.Output += "error: " + friendlyKubeErr(err)
		resp.ExitCode = 1
	}
	if len(resp.Output) > kubectlMaxOutput {
		resp.Output = resp.Output[:kubectlMaxOutput]
		resp.Truncated = true
	}
	httpx.OK(c, resp)
}

func friendlyKubeErr(err error) string {
	if err == context.DeadlineExceeded || strings.Contains(err.Error(), "context deadline exceeded") {
		return "命令执行超时（30 秒）"
	}
	return err.Error()
}

// kubectlChange 把变更命令转成平台变更单。
func (m *Module) kubectlChange(c *gin.Context, p *rbac.Principal, cl *Cluster, a *kubectlArgs, reason string) {
	fail := func(msg string) {
		httpx.OK(c, kubectlResponse{Output: "error: " + msg, ExitCode: 1, Namespace: a.ns})
	}
	pl := changePayload{ClusterID: cl.ID, Cluster: cl.Name}
	var summary string
	switch a.verb {
	case "cordon", "uncordon":
		if len(a.pos) != 1 {
			fail(a.verb + " 需要一个节点名")
			return
		}
		pl.Action, pl.Name = a.verb, a.pos[0]
		summary = "node/" + pl.Name
	case "scale", "delete", "rollout":
		args := a.pos
		action := a.verb
		if a.verb == "rollout" {
			action, args = "restart", a.pos[1:]
		}
		kind, name, err := kindAndName(args)
		if err != nil {
			fail(err.Error())
			return
		}
		if a.allNS {
			fail("变更命令不能与 -A 同时使用")
			return
		}
		pl.Action, pl.Namespace, pl.Kind, pl.Name = action, a.ns, kind, name
		if action == "scale" {
			if a.replicas == nil {
				fail("scale 需要 --replicas=N")
				return
			}
			raw, _ := json.Marshal(scaleParams{Replicas: a.replicas})
			pl.Params = raw
		}
		summary = kind + "/" + name
	}
	if err := validateChange(&pl); err != nil {
		fail(errMessage(err))
		return
	}
	res, t, err := m.runChange(c.Request.Context(), p, cl, pl, reason)
	if err != nil {
		fail(errMessage(err))
		return
	}
	if t != nil {
		httpx.OK(c, kubectlResponse{Output: fmt.Sprintf("%s 已提交变更审批（工单 #%d），审批通过后由平台执行。", summary, t.ID), Namespace: a.ns, Ticket: t, Change: true})
		return
	}
	verbDone := map[string]string{"scale": "scaled", "delete": "deleted", "restart": "restarted", "cordon": "cordoned", "uncordon": "uncordoned"}[pl.Action]
	_ = res // 结果已由变更流程记入审计，终端保持 kubectl 风格的一行输出
	out := summary + " " + verbDone
	httpx.OK(c, kubectlResponse{Output: out, Namespace: a.ns, Change: true})
}

// kindAndName 接受 "deploy/web" 或 "deploy web"。
func kindAndName(args []string) (string, string, error) {
	var k, n string
	switch {
	case len(args) == 1 && strings.Contains(args[0], "/"):
		k, n, _ = strings.Cut(args[0], "/")
	case len(args) == 2:
		k, n = args[0], args[1]
	default:
		return "", "", fmt.Errorf("请指定 <类型>/<名称>，例如 deploy/web")
	}
	nk, err := normalizeKind(k)
	if err != nil {
		return "", "", fmt.Errorf("变更命令仅支持 Deployment、StatefulSet、DaemonSet、Job、CronJob、Pod")
	}
	if n == "" {
		return "", "", fmt.Errorf("缺少资源名称")
	}
	return nk, n, nil
}

func runKubectlRead(ctx context.Context, kc *kubeClients, a *kubectlArgs) (string, error) {
	switch a.verb {
	case "get":
		return kubectlGet(ctx, kc, a)
	case "describe":
		return kubectlDescribe(ctx, kc, a)
	case "logs", "log":
		return kubectlLogs(ctx, kc, a)
	case "top":
		return kubectlTop(ctx, kc, a)
	case "api-resources":
		return kubectlAPIResources(kc)
	case "version":
		v, err := kc.cs.Discovery().ServerVersion()
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Server Version: %s\nPlatform: %s", v.GitVersion, v.Platform), nil
	case "cluster-info":
		v, err := kc.cs.Discovery().ServerVersion()
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Kubernetes control plane is running (%s)", v.GitVersion), nil
	case "rollout":
		return kubectlRolloutRead(ctx, kc, a)
	case "exec":
		return kubectlPodExec(ctx, kc, a)
	case "apply", "create", "edit", "patch", "replace", "label", "annotate", "set":
		return "", fmt.Errorf("%s 不在 Web 终端中执行，请使用资源页面的「YAML 编辑」或「创建」，以便走审批与审计", a.verb)
	case "port-forward", "proxy", "attach", "cp", "debug", "run":
		return "", fmt.Errorf("Web 终端不支持 %s", a.verb)
	}
	return "", fmt.Errorf("不支持的命令 %q，输入 help 查看支持的命令", a.verb)
}

// resolveResource 把 kubectl 资源写法（deploy、po、svc、ingresses.networking.k8s.io）解析为 GVR。
func resolveResource(kc *kubeClients, s string) (schema.GroupVersionResource, bool, error) {
	res, group, _ := strings.Cut(strings.ToLower(s), ".")
	exp := restmapper.NewShortcutExpander(kc.mapper, kc.cs.Discovery(), func(string) {})
	gvr, err := exp.ResourceFor(schema.GroupVersionResource{Group: group, Resource: res})
	if err != nil {
		kc.mapper.Reset()
		if gvr, err = exp.ResourceFor(schema.GroupVersionResource{Group: group, Resource: res}); err != nil {
			return gvr, false, fmt.Errorf(`the server doesn't have a resource type "%s"`, s)
		}
	}
	gvk, err := kc.mapper.KindFor(gvr)
	if err != nil {
		return gvr, false, err
	}
	mp, err := kc.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return gvr, false, err
	}
	return gvr, mp.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

func resourcePath(gvr schema.GroupVersionResource, namespaced bool, ns, name string) string {
	var b strings.Builder
	if gvr.Group == "" {
		b.WriteString("/api/" + gvr.Version)
	} else {
		b.WriteString("/apis/" + gvr.Group + "/" + gvr.Version)
	}
	if namespaced && ns != "" {
		b.WriteString("/namespaces/" + ns)
	}
	b.WriteString("/" + gvr.Resource)
	if name != "" {
		b.WriteString("/" + name)
	}
	return b.String()
}

// tableGet 向 API Server 请求服务端 Table 格式（与 kubectl get 一致的列）。
func tableGet(ctx context.Context, kc *kubeClients, path, selector string) (*metav1.Table, error) {
	rc := rest.CopyConfig(kc.rest)
	rc.GroupVersion = &schema.GroupVersion{Version: "v1"}
	rc.APIPath = "/"
	rc.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	client, err := rest.RESTClientFor(rc)
	if err != nil {
		return nil, err
	}
	req := client.Get().AbsPath(path).
		SetHeader("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io,application/json").
		Param("includeObject", "Metadata")
	if selector != "" {
		req = req.Param("labelSelector", selector)
	}
	raw, err := req.DoRaw(ctx)
	if err != nil {
		return nil, err
	}
	var t metav1.Table
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, err
	}
	if t.Kind != "Table" {
		return nil, fmt.Errorf("API Server 未返回表格格式")
	}
	return &t, nil
}

func kubectlGet(ctx context.Context, kc *kubeClients, a *kubectlArgs) (string, error) {
	if len(a.pos) == 0 {
		return "", fmt.Errorf("You must specify the type of resource to get. 例如：get pods")
	}
	types := strings.Split(a.pos[0], ",")
	names := a.pos[1:]
	if len(types) == 1 && strings.Contains(types[0], "/") && len(names) == 0 {
		t, n, _ := strings.Cut(types[0], "/")
		types, names = []string{t}, []string{n}
	}
	if len(types) > 1 && len(names) > 0 {
		return "", fmt.Errorf("指定名称时只能使用一种资源类型")
	}
	var out bytes.Buffer
	for ti, typ := range types {
		gvr, namespaced, err := resolveResource(kc, typ)
		if err != nil {
			return out.String(), err
		}
		ns := a.ns
		if a.allNS || !namespaced {
			ns = ""
		}
		switch a.output {
		case "yaml", "json", "name":
			if err := writeObjects(ctx, kc, &out, gvr, namespaced, ns, names, a); err != nil {
				return out.String(), err
			}
			continue
		case "", "wide":
		default:
			return "", fmt.Errorf("不支持的输出格式 %q（支持 wide、yaml、json、name）", a.output)
		}
		var tbl *metav1.Table
		if len(names) == 0 {
			tbl, err = tableGet(ctx, kc, resourcePath(gvr, namespaced, ns, ""), a.selector)
			if err != nil {
				return out.String(), err
			}
		} else {
			for _, n := range names {
				t, err := tableGet(ctx, kc, resourcePath(gvr, namespaced, a.ns, n), "")
				if err != nil {
					return out.String(), err
				}
				if tbl == nil {
					tbl = t
				} else {
					tbl.Rows = append(tbl.Rows, t.Rows...)
				}
			}
		}
		if ti > 0 && out.Len() > 0 {
			out.WriteString("\n")
		}
		if tbl == nil || len(tbl.Rows) == 0 {
			if namespaced && !a.allNS {
				fmt.Fprintf(&out, "No resources found in %s namespace.\n", a.ns)
			} else {
				out.WriteString("No resources found\n")
			}
			continue
		}
		prefix := ""
		if len(types) > 1 {
			prefix = gvr.Resource
			if gvr.Group != "" {
				prefix += "." + gvr.Group
			}
			prefix += "/"
		}
		renderTable(&out, tbl, a.output == "wide", a.allNS && namespaced, prefix)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

func renderTable(w io.Writer, t *metav1.Table, wide, withNS bool, namePrefix string) {
	tw := tabwriter.NewWriter(w, 0, 8, 3, ' ', 0)
	var cols []int
	var head []string
	if withNS {
		head = append(head, "NAMESPACE")
	}
	for i, c := range t.ColumnDefinitions {
		if c.Priority == 0 || wide {
			cols = append(cols, i)
			head = append(head, strings.ToUpper(c.Name))
		}
	}
	fmt.Fprintln(tw, strings.Join(head, "\t"))
	for _, r := range t.Rows {
		var cells []string
		if withNS {
			ns := ""
			if len(r.Object.Raw) > 0 {
				var pm metav1.PartialObjectMetadata
				if json.Unmarshal(r.Object.Raw, &pm) == nil {
					ns = pm.Namespace
				}
			}
			cells = append(cells, ns)
		}
		for k, i := range cols {
			v := ""
			if i < len(r.Cells) {
				v = cellString(r.Cells[i])
			}
			if k == 0 && namePrefix != "" {
				v = namePrefix + v
			}
			cells = append(cells, v)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	_ = tw.Flush()
}

func cellString(v any) string {
	switch x := v.(type) {
	case nil:
		return "<none>"
	case string:
		if x == "" {
			return "<none>"
		}
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

// stripObject 去掉 managedFields；Secret 的取值一律隐藏（与平台 Secret 页面一致，查看明文须走资源页并记审计）。
func stripObject(o *unstructured.Unstructured) {
	unstructured.RemoveNestedField(o.Object, "metadata", "managedFields")
	if o.GetKind() == "Secret" {
		for _, f := range []string{"data", "stringData"} {
			if m, ok := o.Object[f].(map[string]any); ok {
				for k := range m {
					m[k] = "<已隐藏>"
				}
			}
		}
		if ann := o.GetAnnotations(); ann != nil {
			delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
			o.SetAnnotations(ann)
		}
	}
}

func writeObjects(ctx context.Context, kc *kubeClients, out *bytes.Buffer, gvr schema.GroupVersionResource, namespaced bool, ns string, names []string, a *kubectlArgs) error {
	ri := kc.dyn.Resource(gvr)
	get := func(n string) (*unstructured.Unstructured, error) {
		if namespaced {
			return ri.Namespace(a.ns).Get(ctx, n, metav1.GetOptions{})
		}
		return ri.Get(ctx, n, metav1.GetOptions{})
	}
	var items []unstructured.Unstructured
	var single *unstructured.Unstructured
	if len(names) > 0 {
		for _, n := range names {
			o, err := get(n)
			if err != nil {
				return err
			}
			items = append(items, *o)
		}
		if len(names) == 1 {
			single = &items[0]
		}
	} else {
		var lst *unstructured.UnstructuredList
		var err error
		opts := metav1.ListOptions{LabelSelector: a.selector}
		if namespaced && ns != "" {
			lst, err = ri.Namespace(ns).List(ctx, opts)
		} else {
			lst, err = ri.List(ctx, opts)
		}
		if err != nil {
			return err
		}
		items = lst.Items
	}
	if a.output == "name" {
		for _, o := range items {
			k := strings.ToLower(o.GetKind())
			if gvr.Group != "" {
				k += "." + gvr.Group
			}
			fmt.Fprintf(out, "%s/%s\n", k, o.GetName())
		}
		return nil
	}
	var doc any
	if single != nil {
		stripObject(single)
		doc = single.Object
	} else {
		list := make([]any, 0, len(items))
		for i := range items {
			stripObject(&items[i])
			list = append(list, items[i].Object)
		}
		doc = map[string]any{"apiVersion": "v1", "kind": "List", "items": list}
	}
	var b []byte
	var err error
	if a.output == "json" {
		b, err = json.MarshalIndent(doc, "", "    ")
	} else {
		b, err = yaml.Marshal(doc)
	}
	if err != nil {
		return err
	}
	out.Write(b)
	if len(b) > 0 && b[len(b)-1] != '\n' {
		out.WriteString("\n")
	}
	return nil
}

func kubectlDescribe(ctx context.Context, kc *kubeClients, a *kubectlArgs) (string, error) {
	args := a.pos
	if len(args) == 1 && strings.Contains(args[0], "/") {
		t, n, _ := strings.Cut(args[0], "/")
		args = []string{t, n}
	}
	if len(args) != 2 {
		return "", fmt.Errorf("用法：describe <资源> <名称>")
	}
	gvr, namespaced, err := resolveResource(kc, args[0])
	if err != nil {
		return "", err
	}
	ri := kc.dyn.Resource(gvr)
	var o *unstructured.Unstructured
	if namespaced {
		o, err = ri.Namespace(a.ns).Get(ctx, args[1], metav1.GetOptions{})
	} else {
		o, err = ri.Get(ctx, args[1], metav1.GetOptions{})
	}
	if err != nil {
		return "", err
	}
	stripObject(o)
	var out bytes.Buffer
	tw := tabwriter.NewWriter(&out, 0, 8, 1, ' ', 0)
	fmt.Fprintf(tw, "Name:\t%s\n", o.GetName())
	if namespaced {
		fmt.Fprintf(tw, "Namespace:\t%s\n", o.GetNamespace())
	}
	fmt.Fprintf(tw, "Kind:\t%s\n", o.GetKind())
	fmt.Fprintf(tw, "Created:\t%s\n", o.GetCreationTimestamp().Format(time.RFC3339))
	fmt.Fprintf(tw, "Labels:\t%s\n", mapLine(o.GetLabels()))
	ann := o.GetAnnotations()
	delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
	fmt.Fprintf(tw, "Annotations:\t%s\n", mapLine(ann))
	_ = tw.Flush()
	for _, sec := range []string{"spec", "status", "data"} {
		v, ok := o.Object[sec]
		if !ok {
			continue
		}
		if sec == "data" && strings.EqualFold(o.GetKind(), "Secret") {
			m, _ := v.(map[string]any)
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			out.WriteString("Data:\n")
			for _, k := range keys {
				fmt.Fprintf(&out, "  %s:  <已隐藏>\n", k)
			}
			continue
		}
		b, err := yaml.Marshal(v)
		if err != nil {
			continue
		}
		out.WriteString(strings.ToUpper(sec[:1]) + sec[1:] + ":\n")
		for _, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			out.WriteString("  " + ln + "\n")
		}
	}
	// Events
	sel := "involvedObject.name=" + o.GetName() + ",involvedObject.uid=" + string(o.GetUID())
	evNS := o.GetNamespace()
	evs, err := kc.cs.CoreV1().Events(evNS).List(ctx, metav1.ListOptions{FieldSelector: sel})
	out.WriteString("Events:")
	if err != nil || len(evs.Items) == 0 {
		out.WriteString("  <none>\n")
	} else {
		out.WriteString("\n")
		sort.Slice(evs.Items, func(i, j int) bool { return eventTime(evs.Items[i]).Before(eventTime(evs.Items[j])) })
		etw := tabwriter.NewWriter(&out, 0, 8, 2, ' ', 0)
		fmt.Fprintln(etw, "  Type\tReason\tAge\tFrom\tMessage")
		for _, e := range evs.Items {
			fmt.Fprintf(etw, "  %s\t%s\t%s\t%s\t%s\n", e.Type, e.Reason, shortAge(eventTime(e)), e.Source.Component, strings.TrimSpace(e.Message))
		}
		_ = etw.Flush()
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

func eventTime(e corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

func shortAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func mapLine(m map[string]string) string {
	if len(m) == 0 {
		return "<none>"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, "\n\t")
}

func kubectlLogs(ctx context.Context, kc *kubeClients, a *kubectlArgs) (string, error) {
	if len(a.pos) != 1 {
		return "", fmt.Errorf("用法：logs <Pod> [-c 容器] [--tail N]")
	}
	name := strings.TrimPrefix(strings.TrimPrefix(a.pos[0], "pod/"), "pods/")
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("Web 终端的 logs 只支持 Pod，请指定 Pod 名称")
	}
	tail := a.tail
	if tail < 0 || tail > 5000 {
		tail = 500
	}
	limit := int64(kubectlMaxOutput)
	opts := &corev1.PodLogOptions{Container: a.container, TailLines: &tail, Previous: a.previous, LimitBytes: &limit}
	b, err := kc.cs.CoreV1().Pods(a.ns).GetLogs(name, opts).DoRaw(ctx)
	if err != nil {
		var st struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &st) == nil && st.Message != "" {
			return "", fmt.Errorf("%s", st.Message)
		}
		return "", err
	}
	return strings.TrimRight(string(b), "\n"), nil
}

func kubectlTop(ctx context.Context, kc *kubeClients, a *kubectlArgs) (string, error) {
	if len(a.pos) == 0 {
		return "", fmt.Errorf("用法：top nodes | top pods")
	}
	if kc.metrics == nil {
		return "", fmt.Errorf("Metrics API not available（集群未安装 metrics-server）")
	}
	var out bytes.Buffer
	tw := tabwriter.NewWriter(&out, 0, 8, 3, ' ', 0)
	switch strings.ToLower(a.pos[0]) {
	case "node", "nodes", "no":
		ms, err := kc.metrics.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{LabelSelector: a.selector})
		if err != nil {
			return "", fmt.Errorf("Metrics API not available: %v", err)
		}
		nodes, _ := kc.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		alloc := map[string]corev1.ResourceList{}
		if nodes != nil {
			for _, n := range nodes.Items {
				alloc[n.Name] = n.Status.Allocatable
			}
		}
		sort.Slice(ms.Items, func(i, j int) bool { return ms.Items[i].Name < ms.Items[j].Name })
		fmt.Fprintln(tw, "NAME\tCPU(cores)\tCPU%\tMEMORY(bytes)\tMEMORY%")
		for _, n := range ms.Items {
			cpu, mem := n.Usage.Cpu(), n.Usage.Memory()
			al := alloc[n.Name]
			fmt.Fprintf(tw, "%s\t%dm\t%s\t%dMi\t%s\n", n.Name, cpu.MilliValue(), pct(cpu.MilliValue(), al.Cpu(), true),
				mem.Value()/(1<<20), pct(mem.Value(), al.Memory(), false))
		}
	case "pod", "pods", "po":
		ns := a.ns
		if a.allNS {
			ns = ""
		}
		ms, err := kc.metrics.MetricsV1beta1().PodMetricses(ns).List(ctx, metav1.ListOptions{LabelSelector: a.selector})
		if err != nil {
			return "", fmt.Errorf("Metrics API not available: %v", err)
		}
		if len(ms.Items) == 0 {
			return fmt.Sprintf("No resources found in %s namespace.", a.ns), nil
		}
		sort.Slice(ms.Items, func(i, j int) bool {
			if ms.Items[i].Namespace != ms.Items[j].Namespace {
				return ms.Items[i].Namespace < ms.Items[j].Namespace
			}
			return ms.Items[i].Name < ms.Items[j].Name
		})
		if a.allNS {
			fmt.Fprintln(tw, "NAMESPACE\tNAME\tCPU(cores)\tMEMORY(bytes)")
		} else {
			fmt.Fprintln(tw, "NAME\tCPU(cores)\tMEMORY(bytes)")
		}
		for _, p := range ms.Items {
			var cpu, mem int64
			for _, c := range p.Containers {
				cpu += c.Usage.Cpu().MilliValue()
				mem += c.Usage.Memory().Value()
			}
			if a.allNS {
				fmt.Fprintf(tw, "%s\t", p.Namespace)
			}
			fmt.Fprintf(tw, "%s\t%dm\t%dMi\n", p.Name, cpu, mem/(1<<20))
		}
	default:
		return "", fmt.Errorf("top 只支持 nodes 与 pods")
	}
	_ = tw.Flush()
	return strings.TrimRight(out.String(), "\n"), nil
}

func pct(v int64, q *resource.Quantity, milli bool) string {
	if q == nil || q.IsZero() {
		return "<unknown>"
	}
	d := q.Value()
	if milli {
		d = q.MilliValue()
	}
	if d == 0 {
		return "<unknown>"
	}
	return fmt.Sprintf("%d%%", v*100/d)
}

func kubectlAPIResources(kc *kubeClients) (string, error) {
	_, lists, err := kc.cs.Discovery().ServerGroupsAndResources()
	if err != nil && len(lists) == 0 {
		return "", err
	}
	type row struct {
		name, short, gv, kind string
		ns                    bool
	}
	var rows []row
	for _, l := range lists {
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") {
				continue
			}
			rows = append(rows, row{r.Name, strings.Join(r.ShortNames, ","), l.GroupVersion, r.Kind, r.Namespaced})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	var out bytes.Buffer
	tw := tabwriter.NewWriter(&out, 0, 8, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSHORTNAMES\tAPIVERSION\tNAMESPACED\tKIND")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\n", r.name, r.short, r.gv, r.ns, r.kind)
	}
	_ = tw.Flush()
	return strings.TrimRight(out.String(), "\n"), nil
}

func kubectlRolloutRead(ctx context.Context, kc *kubeClients, a *kubectlArgs) (string, error) {
	if len(a.pos) < 2 {
		return "", fmt.Errorf("用法：rollout status|history <类型>/<名称>")
	}
	sub := a.pos[0]
	kind, name, err := kindAndName(a.pos[1:])
	if err != nil {
		return "", err
	}
	switch sub {
	case "status":
		switch kind {
		case "deployments":
			d, err := kc.cs.AppsV1().Deployments(a.ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return "", err
			}
			want := int32(1)
			if d.Spec.Replicas != nil {
				want = *d.Spec.Replicas
			}
			if d.Status.ObservedGeneration >= d.Generation && d.Status.UpdatedReplicas == want && d.Status.AvailableReplicas == want && d.Status.Replicas == want {
				return fmt.Sprintf("deployment %q successfully rolled out", name), nil
			}
			return fmt.Sprintf("Waiting for deployment %q rollout to finish: %d of %d updated replicas are available...", name, d.Status.AvailableReplicas, want), nil
		case "statefulsets":
			s, err := kc.cs.AppsV1().StatefulSets(a.ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return "", err
			}
			want := int32(1)
			if s.Spec.Replicas != nil {
				want = *s.Spec.Replicas
			}
			if s.Status.UpdatedReplicas == want && s.Status.ReadyReplicas == want {
				return fmt.Sprintf("partitioned roll out complete: %d new pods have been updated...", want), nil
			}
			return fmt.Sprintf("Waiting for %d pods to be ready... (%d ready)", want, s.Status.ReadyReplicas), nil
		case "daemonsets":
			ds, err := kc.cs.AppsV1().DaemonSets(a.ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return "", err
			}
			if ds.Status.UpdatedNumberScheduled == ds.Status.DesiredNumberScheduled && ds.Status.NumberAvailable == ds.Status.DesiredNumberScheduled {
				return fmt.Sprintf("daemon set %q successfully rolled out", name), nil
			}
			return fmt.Sprintf("Waiting for daemon set %q rollout to finish: %d of %d updated pods are available...", name, ds.Status.NumberAvailable, ds.Status.DesiredNumberScheduled), nil
		}
		return "", fmt.Errorf("rollout status 只支持 Deployment、StatefulSet、DaemonSet")
	case "history":
		if kind != "deployments" {
			return "", fmt.Errorf("rollout history 目前只支持 Deployment")
		}
		d, err := kc.cs.AppsV1().Deployments(a.ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
		if err != nil {
			return "", err
		}
		rss, err := kc.cs.AppsV1().ReplicaSets(a.ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
		if err != nil {
			return "", err
		}
		type rev struct {
			n     int64
			cause string
		}
		var revs []rev
		for _, rs := range rss.Items {
			owned := false
			for _, o := range rs.OwnerReferences {
				if o.UID == d.UID {
					owned = true
				}
			}
			if !owned {
				continue
			}
			n, _ := strconv.ParseInt(rs.Annotations["deployment.kubernetes.io/revision"], 10, 64)
			cause := rs.Annotations["kubernetes.io/change-cause"]
			if cause == "" {
				cause = "<none>"
			}
			revs = append(revs, rev{n, cause})
		}
		sort.Slice(revs, func(i, j int) bool { return revs[i].n < revs[j].n })
		var out bytes.Buffer
		fmt.Fprintf(&out, "deployment.apps/%s\n", name)
		tw := tabwriter.NewWriter(&out, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "REVISION\tCHANGE-CAUSE")
		for _, r := range revs {
			fmt.Fprintf(tw, "%d\t%s\n", r.n, r.cause)
		}
		_ = tw.Flush()
		return strings.TrimRight(out.String(), "\n"), nil
	case "undo", "pause", "resume":
		return "", fmt.Errorf("rollout %s 请在工作负载详情页的「版本记录」中操作，以便走审批", sub)
	}
	return "", fmt.Errorf("不支持的 rollout 子命令 %q", sub)
}

// kubectlPodExec 以非交互方式在容器内执行一条命令。
func kubectlPodExec(ctx context.Context, kc *kubeClients, a *kubectlArgs) (string, error) {
	if len(a.pos) != 1 || len(a.execArgs) == 0 {
		return "", fmt.Errorf("用法：exec <Pod> [-c 容器] -- <命令> [参数...]")
	}
	pod := strings.TrimPrefix(strings.TrimPrefix(a.pos[0], "pod/"), "pods/")
	req := kc.cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(a.ns).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: a.container, Command: a.execArgs, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	ex, err := remotecommand.NewSPDYExecutor(streamConfig(kc.rest), http.MethodPost, req.URL())
	if err != nil {
		return "", err
	}
	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = kubectlMaxOutput, 64<<10
	err = ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	out := stdout.String() + stderr.String()
	out = strings.TrimRight(out, "\n")
	if err != nil {
		if strings.Contains(err.Error(), "command terminated with exit code") {
			return out, fmt.Errorf("%s", err.Error())
		}
		return out, err
	}
	return out, nil
}

type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}

// errMessage 只返回面向用户的错误信息，不泄露内部原因。
func errMessage(err error) string {
	var he *httpx.Error
	if errors.As(err, &he) {
		return he.Message
	}
	return err.Error()
}
