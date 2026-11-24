package aiops

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func k8sInt32p(v int32) *int32 { return &v }

func k8sTestDeployment(name string, replicas int32, image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: "dep-uid",
			Annotations: map[string]string{k8sRevisionAnno: "3"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: k8sInt32p(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: image,
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}}}}},
			},
		},
	}
}

func k8sTestRS(dep *appsv1.Deployment, rev, image string) *appsv1.ReplicaSet {
	t := dep.Spec.Template.DeepCopy()
	t.Labels = map[string]string{"app": dep.Name, "pod-template-hash": "h" + rev}
	t.Spec.Containers[0].Image = image
	ctrl := true
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: dep.Name + "-h" + rev, Namespace: "default", Labels: t.Labels,
			Annotations:     map[string]string{k8sRevisionAnno: rev},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: dep.Name, UID: dep.UID, Controller: &ctrl}}},
		Spec: appsv1.ReplicaSetSpec{Template: *t},
	}
}

func k8sTestPod(name, node string, owner string, mods ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID("uid-" + name), Labels: map[string]string{"app": "web"}},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true}}},
	}
	if owner != "" {
		ctrl := true
		kind, oname, _ := strings.Cut(owner, "/")
		p.OwnerReferences = []metav1.OwnerReference{{Kind: kind, Name: oname, Controller: &ctrl, UID: "o"}}
	}
	for _, m := range mods {
		m(p)
	}
	return p
}

func k8sUseFakeK8s(t *testing.T, objs ...runtime.Object) *fake.Clientset {
	t.Helper()
	cs := fake.NewSimpleClientset(objs...)
	oldC, oldP := k8sClientFor, k8sPollInterval
	k8sClientFor = func(context.Context, *ExecContext) (kubernetes.Interface, error) { return cs, nil }
	k8sPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { k8sClientFor, k8sPollInterval = oldC, oldP })
	return cs
}

func k8sMustAction(t *testing.T, name string) Action {
	t.Helper()
	a, ok := ActionByName(name)
	if !ok {
		t.Fatalf("action %s not registered", name)
	}
	return a
}

var k8sEC = &ExecContext{ClusterID: 1, Env: "test"}

func TestK8sActionsRegistered(t *testing.T) {
	want := map[string]string{
		"k8s.get_pods": RiskRead, "k8s.describe": RiskRead, "k8s.logs": RiskRead, "k8s.events": RiskRead, "k8s.wait_ready": RiskRead,
		"k8s.restart": RiskChange, "k8s.scale": RiskChange, "k8s.set_resources": RiskChange, "k8s.set_image": RiskChange, "k8s.rollback_deployment": RiskChange,
		"k8s.cordon": RiskHigh, "k8s.uncordon": RiskHigh, "k8s.drain": RiskHigh, "k8s.delete_pod": RiskHigh, "k8s.apply_yaml": RiskHigh,
	}
	for name, risk := range want {
		a := k8sMustAction(t, name)
		sp := a.Spec()
		if sp.Risk != risk || sp.Channel != "k8s" || sp.ReadOnly != (risk == RiskRead) || sp.Permission == "" {
			t.Errorf("%s spec = %+v", name, sp)
		}
		var schema map[string]any
		if err := json.Unmarshal(sp.Params, &schema); err != nil {
			t.Errorf("%s params schema invalid: %v", name, err)
		}
	}
}

func TestK8sValidate(t *testing.T) {
	prod := &ExecContext{ClusterID: 1, Env: "prod"}
	cases := []struct {
		name, action, params string
		ec                   *ExecContext
		risk, errHas         string
	}{
		{"pods ok", "k8s.get_pods", `{}`, nil, RiskRead, ""},
		{"pods bad selector", "k8s.get_pods", `{"selector":"a in (("}`, nil, "", "标签选择器"},
		{"unknown field", "k8s.scale", `{"kind":"deployment","name":"web","replicas":1,"foo":1}`, nil, "", "参数格式错误"},
		{"bad kind", "k8s.restart", `{"kind":"cronjob","name":"web"}`, nil, "", "不支持的资源类型"},
		{"pod kind not allowed", "k8s.restart", `{"kind":"pod","name":"web"}`, nil, "", "不支持资源类型"},
		{"missing name", "k8s.restart", `{"kind":"deploy"}`, nil, "", "缺少参数 name"},
		{"bad name", "k8s.restart", `{"kind":"deploy","name":"Web_1"}`, nil, "", "不合法"},
		{"restart ok", "k8s.restart", `{"kind":"deploy","name":"web"}`, k8sEC, RiskChange, ""},
		{"restart prod -> high", "k8s.restart", `{"kind":"deploy","name":"web"}`, prod, RiskHigh, ""},
		{"scale missing replicas", "k8s.scale", `{"kind":"deployment","name":"web"}`, nil, "", "缺少参数 replicas"},
		{"scale too many", "k8s.scale", `{"kind":"deployment","name":"web","replicas":5000}`, nil, "", "副本数须在"},
		{"scale to zero -> high", "k8s.scale", `{"kind":"deployment","name":"web","replicas":0}`, k8sEC, RiskHigh, ""},
		{"scale daemonset rejected", "k8s.scale", `{"kind":"ds","name":"web","replicas":1}`, nil, "", "不支持资源类型"},
		{"resources empty", "k8s.set_resources", `{"kind":"deployment","name":"web"}`, nil, "", "至少填写一项"},
		{"resources bad qty", "k8s.set_resources", `{"kind":"deployment","name":"web","limits":{"cpu":"lots"}}`, nil, "", "不是合法的资源数量"},
		{"resources bad key", "k8s.set_resources", `{"kind":"deployment","name":"web","limits":{"gpu":"1"}}`, nil, "", "只支持"},
		{"resources req>lim", "k8s.set_resources", `{"kind":"deployment","name":"web","requests":{"memory":"1Gi"},"limits":{"memory":"512Mi"}}`, nil, "", "不能大于"},
		{"image missing", "k8s.set_image", `{"kind":"deployment","name":"web"}`, nil, "", "缺少参数 image"},
		{"image bad", "k8s.set_image", `{"kind":"deployment","name":"web","image":"nginx; rm -rf /"}`, nil, "", "镜像地址"},
		{"logs tail too big", "k8s.logs", `{"name":"web-1","tail_lines":500}`, nil, "", "tail_lines"},
		{"events bad type", "k8s.events", `{"type":"Error"}`, nil, "", "Normal 或 Warning"},
		{"wait timeout bad", "k8s.wait_ready", `{"kind":"deployment","name":"web","timeout_sec":1}`, nil, "", "超时时间"},
		{"drain ok", "k8s.drain", `{"name":"node-1"}`, k8sEC, RiskHigh, ""},
		{"delete pod neg grace", "k8s.delete_pod", `{"name":"web-1","grace_period_sec":-1}`, nil, "", "不能为负数"},
		{"apply empty", "k8s.apply_yaml", `{"yaml":"  "}`, nil, "", "缺少参数 yaml"},
		{"apply secret", "k8s.apply_yaml", `{"yaml":"apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\n"}`, nil, "", "不允许"},
		{"apply no name", "k8s.apply_yaml", `{"yaml":"apiVersion: v1\nkind: ConfigMap\n"}`, nil, "", "缺少 metadata.name"},
		{"apply ok", "k8s.apply_yaml", `{"yaml":"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n"}`, k8sEC, RiskHigh, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := k8sMustAction(t, c.action)
			norm, risk, err := a.Validate(context.Background(), c.ec, json.RawMessage(c.params))
			if c.errHas != "" {
				if err == nil || !strings.Contains(err.Error(), c.errHas) {
					t.Fatalf("err = %v, want containing %q", err, c.errHas)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if risk != c.risk {
				t.Fatalf("risk = %s, want %s", risk, c.risk)
			}
			if !json.Valid(norm) {
				t.Fatalf("normalized params invalid: %s", norm)
			}
		})
	}
}

func TestK8sNoClusterError(t *testing.T) {
	_, err := k8sMustAction(t, "k8s.get_pods").Execute(context.Background(), &ExecContext{}, json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "集群") {
		t.Fatalf("err = %v", err)
	}
}

func TestK8sScaleExecuteRollback(t *testing.T) {
	cs := k8sUseFakeK8s(t, k8sTestDeployment("web", 3, "nginx:1.25"))
	a := k8sMustAction(t, "k8s.scale")
	params := json.RawMessage(`{"kind":"deployment","name":"web","replicas":5}`)
	_, preview, manual, err := a.Describe(context.Background(), k8sEC, params)
	if err != nil || !strings.Contains(preview, "3 → 5") || !strings.Contains(manual, "--replicas=5") {
		t.Fatalf("describe: %q %q %v", preview, manual, err)
	}
	out, err := a.Execute(context.Background(), k8sEC, params)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := cs.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	if *d.Spec.Replicas != 5 {
		t.Fatalf("replicas = %d", *d.Spec.Replicas)
	}
	if _, err := a.Rollback(context.Background(), k8sEC, params, out); err != nil {
		t.Fatal(err)
	}
	d, _ = cs.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	if *d.Spec.Replicas != 3 {
		t.Fatalf("after rollback replicas = %d", *d.Spec.Replicas)
	}
	if _, err := a.Rollback(context.Background(), k8sEC, params, "no state"); err == nil {
		t.Fatal("rollback without state should fail")
	}
}

func TestK8sNotFoundChinese(t *testing.T) {
	k8sUseFakeK8s(t)
	_, err := k8sMustAction(t, "k8s.scale").Execute(context.Background(), k8sEC, json.RawMessage(`{"kind":"deployment","name":"nope","replicas":1}`))
	if err == nil || !strings.Contains(err.Error(), "未找到") {
		t.Fatalf("err = %v", err)
	}
}

func TestK8sSetImageAndResources(t *testing.T) {
	cs := k8sUseFakeK8s(t, k8sTestDeployment("web", 2, "nginx:1.25"))
	ctx := context.Background()
	img := k8sMustAction(t, "k8s.set_image")
	p := json.RawMessage(`{"kind":"deployment","name":"web","image":"nginx:1.27"}`)
	_, preview, _, err := img.Describe(ctx, k8sEC, p)
	if err != nil || !strings.Contains(preview, "- nginx:1.25") || !strings.Contains(preview, "+ nginx:1.27") {
		t.Fatalf("preview %q %v", preview, err)
	}
	out, err := img.Execute(ctx, k8sEC, p)
	if err != nil {
		t.Fatal(err)
	}
	get := func() *appsv1.Deployment {
		d, _ := cs.AppsV1().Deployments("default").Get(ctx, "web", metav1.GetOptions{})
		return d
	}
	if got := get().Spec.Template.Spec.Containers[0].Image; got != "nginx:1.27" {
		t.Fatalf("image = %s", got)
	}
	if _, err := img.Rollback(ctx, k8sEC, p, out); err != nil {
		t.Fatal(err)
	}
	if got := get().Spec.Template.Spec.Containers[0].Image; got != "nginx:1.25" {
		t.Fatalf("image after rollback = %s", got)
	}

	res := k8sMustAction(t, "k8s.set_resources")
	rp := json.RawMessage(`{"kind":"deployment","name":"web","container":"app","requests":{"cpu":"200m"},"limits":{"memory":"512Mi"}}`)
	_, preview, _, err = res.Describe(ctx, k8sEC, rp)
	if err != nil || !strings.Contains(preview, "requests.cpu 100m → 200m") || !strings.Contains(preview, "limits.memory 未设置 → 512Mi") {
		t.Fatalf("preview %q %v", preview, err)
	}
	out, err = res.Execute(ctx, k8sEC, rp)
	if err != nil {
		t.Fatal(err)
	}
	c := get().Spec.Template.Spec.Containers[0]
	if c.Resources.Requests.Cpu().String() != "200m" || c.Resources.Limits.Memory().String() != "512Mi" {
		t.Fatalf("resources = %+v", c.Resources)
	}
	if _, err := res.Rollback(ctx, k8sEC, rp, out); err != nil {
		t.Fatal(err)
	}
	c = get().Spec.Template.Spec.Containers[0]
	if c.Resources.Requests.Cpu().String() != "100m" || c.Resources.Limits != nil && !c.Resources.Limits.Memory().IsZero() {
		t.Fatalf("resources after rollback = %+v", c.Resources)
	}
	// request above existing limit is rejected at execute time
	_, err = res.Execute(ctx, k8sEC, json.RawMessage(`{"kind":"deployment","name":"web","limits":{"cpu":"50m"}}`))
	if err == nil || !strings.Contains(err.Error(), "不能大于") {
		t.Fatalf("err = %v", err)
	}
	// unknown container
	_, err = img.Execute(ctx, k8sEC, json.RawMessage(`{"kind":"deployment","name":"web","container":"side","image":"x:1"}`))
	if err == nil || !strings.Contains(err.Error(), "未找到容器") {
		t.Fatalf("err = %v", err)
	}
}

func TestK8sRestartNoRollback(t *testing.T) {
	cs := k8sUseFakeK8s(t, k8sTestDeployment("web", 2, "nginx"))
	a := k8sMustAction(t, "k8s.restart")
	p := json.RawMessage(`{"kind":"deployment","name":"web"}`)
	out, err := a.Execute(context.Background(), k8sEC, p)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := cs.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	if d.Spec.Template.Annotations[k8sRestartAnnotation] == "" {
		t.Fatal("restart annotation missing")
	}
	if _, err := a.Rollback(context.Background(), k8sEC, p, out); !errors.Is(err, ErrNoRollback) {
		t.Fatalf("rollback err = %v", err)
	}
}

func TestK8sRollbackDeployment(t *testing.T) {
	dep := k8sTestDeployment("web", 2, "web:v3")
	cs := k8sUseFakeK8s(t, dep, k8sTestRS(dep, "1", "web:v1"), k8sTestRS(dep, "2", "web:v2"), k8sTestRS(dep, "3", "web:v3"))
	ctx := context.Background()
	a := k8sMustAction(t, "k8s.rollback_deployment")
	p := json.RawMessage(`{"name":"web"}`)
	_, preview, _, err := a.Describe(ctx, k8sEC, p)
	if err != nil || !strings.Contains(preview, "第 3 版 → 第 2 版") || !strings.Contains(preview, "web:v3 → web:v2") {
		t.Fatalf("preview %q %v", preview, err)
	}
	out, err := a.Execute(ctx, k8sEC, p)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := cs.AppsV1().Deployments("default").Get(ctx, "web", metav1.GetOptions{})
	if d.Spec.Template.Spec.Containers[0].Image != "web:v2" || d.Spec.Template.Labels["pod-template-hash"] != "" {
		t.Fatalf("template = %+v", d.Spec.Template)
	}
	if _, err := a.Rollback(ctx, k8sEC, p, out); err != nil {
		t.Fatal(err)
	}
	d, _ = cs.AppsV1().Deployments("default").Get(ctx, "web", metav1.GetOptions{})
	if d.Spec.Template.Spec.Containers[0].Image != "web:v3" {
		t.Fatalf("after undo image = %s", d.Spec.Template.Spec.Containers[0].Image)
	}
	// explicit revision
	if _, err := a.Execute(ctx, k8sEC, json.RawMessage(`{"name":"web","revision":1}`)); err != nil {
		t.Fatal(err)
	}
	d, _ = cs.AppsV1().Deployments("default").Get(ctx, "web", metav1.GetOptions{})
	if d.Spec.Template.Spec.Containers[0].Image != "web:v1" {
		t.Fatalf("image = %s", d.Spec.Template.Spec.Containers[0].Image)
	}
	_, err = a.Execute(ctx, k8sEC, json.RawMessage(`{"name":"web","revision":9}`))
	if err == nil || !strings.Contains(err.Error(), "未找到第 9 版") {
		t.Fatalf("err = %v", err)
	}
}

func TestK8sCordonUncordon(t *testing.T) {
	cs := k8sUseFakeK8s(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}})
	ctx := context.Background()
	p := json.RawMessage(`{"name":"node-1"}`)
	unsched := func() bool {
		n, _ := cs.CoreV1().Nodes().Get(ctx, "node-1", metav1.GetOptions{})
		return n.Spec.Unschedulable
	}
	cordon := k8sMustAction(t, "k8s.cordon")
	out, err := cordon.Execute(ctx, k8sEC, p)
	if err != nil || !unsched() {
		t.Fatalf("cordon: %v unsched=%v", err, unsched())
	}
	if _, err := cordon.Rollback(ctx, k8sEC, p, out); err != nil || unsched() {
		t.Fatalf("cordon rollback: %v", err)
	}
	cordon.Execute(ctx, k8sEC, p)
	un := k8sMustAction(t, "k8s.uncordon")
	out, err = un.Execute(ctx, k8sEC, p)
	if err != nil || unsched() {
		t.Fatalf("uncordon: %v", err)
	}
	if _, err := un.Rollback(ctx, k8sEC, p, out); err != nil || !unsched() {
		t.Fatalf("uncordon rollback: %v", err)
	}
}

// k8sEvictionReactor deletes the pod like the API server would; failFirst simulates a PDB 429.
func k8sEvictionReactor(cs *fake.Clientset, failFirst map[string]int) {
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ca, ok := action.(k8stesting.CreateAction)
		if !ok || action.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		ev := ca.GetObject().(*policyv1.Eviction)
		if failFirst[ev.Name] > 0 {
			failFirst[ev.Name]--
			return true, nil, apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 0)
		}
		gvr := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
		if err := cs.Tracker().Delete(gvr, action.GetNamespace(), ev.Name); err != nil {
			return true, nil, err
		}
		return true, nil, nil
	})
}

func TestK8sDrain(t *testing.T) {
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	cs := k8sUseFakeK8s(t, node,
		k8sTestPod("web-1", "node-1", "ReplicaSet/web-abc"),
		k8sTestPod("web-2", "node-1", "ReplicaSet/web-abc"),
		k8sTestPod("agent-x", "node-1", "DaemonSet/agent"),
		k8sTestPod("static", "node-1", "", func(p *corev1.Pod) { p.Annotations = map[string]string{k8sMirrorAnno: "x"} }),
		k8sTestPod("other", "node-2", "ReplicaSet/web-abc"),
	)
	k8sEvictionReactor(cs, map[string]int{"web-2": 2}) // PDB blocks web-2 twice
	a := k8sMustAction(t, "k8s.drain")
	p := json.RawMessage(`{"name":"node-1","timeout_sec":30}`)
	_, preview, manual, err := a.Describe(ctx, k8sEC, p)
	if err != nil || !strings.Contains(preview, "将驱逐 2 个 Pod") || !strings.Contains(preview, "DaemonSet") || !strings.Contains(manual, "--ignore-daemonsets") {
		t.Fatalf("preview %q %v", preview, err)
	}
	out, err := a.Execute(ctx, k8sEC, p)
	if err != nil {
		t.Fatalf("drain: %v\n%s", err, out)
	}
	for _, name := range []string{"web-1", "web-2"} {
		if _, err := cs.CoreV1().Pods("default").Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s still present", name)
		}
	}
	for _, name := range []string{"agent-x", "static", "other"} {
		if _, err := cs.CoreV1().Pods("default").Get(ctx, name, metav1.GetOptions{}); err != nil {
			t.Errorf("%s should not be evicted: %v", name, err)
		}
	}
	n, _ := cs.CoreV1().Nodes().Get(ctx, "node-1", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Fatal("node not cordoned")
	}
	if _, err := a.Rollback(ctx, k8sEC, p, out); err != nil {
		t.Fatal(err)
	}
	n, _ = cs.CoreV1().Nodes().Get(ctx, "node-1", metav1.GetOptions{})
	if n.Spec.Unschedulable {
		t.Fatal("rollback did not uncordon")
	}
}

func TestK8sDrainBlocked(t *testing.T) {
	ctx := context.Background()
	cs := k8sUseFakeK8s(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		k8sTestPod("bare", "node-1", ""),
		k8sTestPod("cache", "node-1", "ReplicaSet/c", func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		}),
	)
	a := k8sMustAction(t, "k8s.drain")
	_, err := a.Execute(ctx, k8sEC, json.RawMessage(`{"name":"node-1"}`))
	if err == nil || !strings.Contains(err.Error(), "阻止排空") || !strings.Contains(err.Error(), "emptyDir") || !strings.Contains(err.Error(), "force=true") {
		t.Fatalf("err = %v", err)
	}
	n, _ := cs.CoreV1().Nodes().Get(ctx, "node-1", metav1.GetOptions{})
	if n.Spec.Unschedulable {
		t.Fatal("blocked drain must not cordon")
	}
}

func TestK8sDeletePod(t *testing.T) {
	ctx := context.Background()
	cs := k8sUseFakeK8s(t, k8sTestPod("web-1", "node-1", "ReplicaSet/web-abc"), k8sTestPod("bare", "node-1", ""))
	a := k8sMustAction(t, "k8s.delete_pod")
	_, preview, _, err := a.Describe(ctx, k8sEC, json.RawMessage(`{"name":"bare"}`))
	if err != nil || !strings.Contains(preview, "不会重建") {
		t.Fatalf("preview %q %v", preview, err)
	}
	p := json.RawMessage(`{"name":"web-1"}`)
	out, err := a.Execute(ctx, k8sEC, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Pods("default").Get(ctx, "web-1", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("pod not deleted")
	}
	if _, err := a.Rollback(ctx, k8sEC, p, out); !errors.Is(err, ErrNoRollback) {
		t.Fatalf("rollback err = %v", err)
	}
}

func TestK8sReadActions(t *testing.T) {
	ctx := context.Background()
	dep := k8sTestDeployment("web", 2, "nginx")
	crash := k8sTestPod("web-2", "node-1", "ReplicaSet/web-abc", func(p *corev1.Pod) {
		p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "DB_PASSWORD", Value: "hunter2"}, {Name: "MODE", Value: "prod"}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 7,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
	})
	ev := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: "default"}, Type: "Warning", Reason: "BackOff",
		Message: "Back-off restarting failed container", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-2"},
		LastTimestamp: metav1.NewTime(time.Now())}
	ev2 := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "e2", Namespace: "default"}, Type: "Normal", Reason: "Pulled",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-1"}}
	k8sUseFakeK8s(t, dep, k8sTestPod("web-1", "node-1", "ReplicaSet/web-abc"), crash, ev, ev2)

	cases := []struct {
		action, params string
		has            []string
		not            []string
	}{
		{"k8s.get_pods", `{}`, []string{"web-1", "CrashLoopBackOff", "共 2 个 Pod，未就绪 1 个"}, nil},
		{"k8s.get_pods", `{"kind":"deployment","name":"web"}`, []string{"web-2"}, nil},
		{"k8s.events", `{"name":"web-2","kind":"pod"}`, []string{"BackOff"}, []string{"Pulled"}},
		{"k8s.events", `{"type":"Warning"}`, []string{"BackOff"}, []string{"Pulled"}},
		{"k8s.describe", `{"kind":"pod","name":"web-2"}`, []string{"DB_PASSWORD", "***", "MODE", "BackOff"}, []string{"hunter2"}},
		{"k8s.logs", `{"name":"web-1"}`, []string{"fake logs"}, nil},
	}
	for _, c := range cases {
		t.Run(c.action+c.params, func(t *testing.T) {
			a := k8sMustAction(t, c.action)
			_, preview, manual, err := a.Describe(ctx, k8sEC, json.RawMessage(c.params))
			if err != nil || !strings.Contains(preview, "只读") || !strings.HasPrefix(manual, "kubectl") {
				t.Fatalf("describe %q %q %v", preview, manual, err)
			}
			out, err := a.Execute(ctx, k8sEC, json.RawMessage(c.params))
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range c.has {
				if !strings.Contains(out, h) {
					t.Errorf("output missing %q:\n%s", h, out)
				}
			}
			for _, n := range c.not {
				if strings.Contains(out, n) {
					t.Errorf("output must not contain %q:\n%s", n, out)
				}
			}
			if strings.Contains(out, k8sStateMarker) {
				t.Error("read action must not record state")
			}
		})
	}
}

func TestK8sWaitReady(t *testing.T) {
	ctx := context.Background()
	ready := k8sTestDeployment("web", 2, "nginx")
	ready.Status = appsv1.DeploymentStatus{Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2}
	slow := k8sTestDeployment("slow", 2, "nginx")
	slow.Status = appsv1.DeploymentStatus{Replicas: 2, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	k8sUseFakeK8s(t, ready, slow)
	a := k8sMustAction(t, "k8s.wait_ready")
	out, err := a.Execute(ctx, k8sEC, json.RawMessage(`{"kind":"deployment","name":"web","timeout_sec":5}`))
	if err != nil || !strings.Contains(out, "已就绪") {
		t.Fatalf("%q %v", out, err)
	}
	start := time.Now()
	_, err = a.Execute(ctx, k8sEC, json.RawMessage(`{"kind":"deployment","name":"slow","timeout_sec":5}`))
	if err == nil || !strings.Contains(err.Error(), "超时") || !strings.Contains(err.Error(), "就绪 1") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("wait_ready did not honour timeout")
	}
}

func TestK8sHelpers(t *testing.T) {
	if got := k8sMaskSecrets(`password=abc token: "xyz" Authorization: Bearer eyJhbGci.x.y ok=1`); strings.Contains(got, "abc") || strings.Contains(got, "xyz") || strings.Contains(got, "eyJ") || !strings.Contains(got, "ok=1") {
		t.Errorf("k8sMaskSecrets = %q", got)
	}
	d := k8sLineDiff("a\nb\nc\n", "a\nc\nd\n")
	if !strings.Contains(d, "- b") || !strings.Contains(d, "+ d") || strings.Contains(d, "a") {
		t.Errorf("k8sLineDiff = %q", d)
	}
	objs, err := k8sParseManifests("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n---\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: b\n  namespace: x\n", "ns1")
	if err != nil || len(objs) != 2 || objs[0].GetNamespace() != "ns1" || objs[1].GetNamespace() != "x" {
		t.Fatalf("k8sParseManifests = %v %v", objs, err)
	}
	if st, ok := k8sExtractState("done" + k8sStateMarker + `{"replicas":3}`); !ok || string(st) != `{"replicas":3}` {
		t.Errorf("k8sExtractState = %s %v", st, ok)
	}
	if StripK8sState("done"+k8sStateMarker+`{}`) != "done" {
		t.Error("StripK8sState")
	}
	if s := k8sTruncateOutput(strings.Repeat("中", 10), 7); !strings.HasPrefix(s, "中中") || strings.ContainsRune(s, '\uFFFD') {
		t.Errorf("truncate = %q", s)
	}
}
