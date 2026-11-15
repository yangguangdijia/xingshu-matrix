package cicd

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

func canaryFixture() (*fake.Clientset, canaryTarget) {
	replicas := int32(3)
	class := "nginx"
	prefix := networkingv1.PathTypePrefix
	stable := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop", Labels: map[string]string{"app": "api"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api", "tier": "web"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api", "tier": "web", "team": "a"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{
					{Name: "api", Image: "reg/api:v1"}, {Name: "sidecar", Image: "reg/proxy:1"}}},
			},
		},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Selector: map[string]string{"app": "api"},
			Ports: []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080), NodePort: 30080}}},
	}
	path := func(p, svc string) networkingv1.HTTPIngressPath {
		return networkingv1.HTTPIngressPath{Path: p, PathType: &prefix, Backend: networkingv1.IngressBackend{
			Service: &networkingv1.IngressServiceBackend{Name: svc, Port: networkingv1.ServiceBackendPort{Number: 80}}}}
	}
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop", Annotations: map[string]string{"nginx.ingress.kubernetes.io/rewrite-target": "/"}},
		Spec: networkingv1.IngressSpec{IngressClassName: &class,
			TLS: []networkingv1.IngressTLS{{Hosts: []string{"shop.example.com"}, SecretName: "tls"}},
			Rules: []networkingv1.IngressRule{
				{Host: "shop.example.com", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{path("/api", "api"), path("/", "frontend")}}}},
				{Host: "admin.example.com", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{path("/", "admin")}}}},
			}},
	}
	cs := fake.NewSimpleClientset(stable, svc, ing)
	t := canaryTarget{ID: 7, Namespace: "shop", Deployment: "api", Container: "api", Service: "api", Ingress: "web",
		CanaryDeployment: "api-canary", CanaryService: "api-canary", CanaryIngress: "web-canary", Image: "reg/api:v2", Replicas: 1}
	return cs, t
}

func canaryWeightOf(t *testing.T, cs *fake.Clientset, tg canaryTarget) string {
	t.Helper()
	ing, err := cs.NetworkingV1().Ingresses(tg.Namespace).Get(context.Background(), tg.CanaryIngress, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get canary ingress: %v", err)
	}
	if ing.Annotations[annCanary] != "true" {
		t.Fatalf("canary annotation = %q", ing.Annotations[annCanary])
	}
	return ing.Annotations[annCanaryWeight]
}

func TestCanaryPlanNormalize(t *testing.T) {
	p := canaryPlan{}
	if err := p.normalize(); err != nil {
		t.Fatal(err)
	}
	if len(p.Steps) != 4 || p.Steps[0].Weight != 10 || p.Steps[3].Weight != 100 || p.Replicas != 1 {
		t.Fatalf("defaults = %+v", p)
	}
	bad := []canaryPlan{
		{Steps: []canaryStep{{Weight: 30}, {Weight: 10}}},
		{Steps: []canaryStep{{Weight: 0}}},
		{Steps: []canaryStep{{Weight: 101}}},
		{Steps: []canaryStep{{Weight: 10, PauseSec: -1}}},
		{Steps: []canaryStep{{Weight: 10}}, Replicas: 51},
		{Steps: make([]canaryStep, 21)},
	}
	for i, b := range bad {
		if err := b.normalize(); err == nil {
			t.Fatalf("plan %d accepted: %+v", i, b)
		}
	}
}

func TestCanaryPlanTransitions(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	p := canaryPlan{Steps: []canaryStep{{Weight: 10, PauseSec: 60}, {Weight: 50, Manual: true}, {Weight: 100, PauseSec: 30}}}
	st := p.enter(0, now)
	if st.Status != canaryProgressing || st.Weight != 10 || !st.NextAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("step 1 = %+v", st)
	}
	if next, promote, wait := p.afterStep(0); next != 1 || promote || wait {
		t.Fatalf("after step 1 = %d %v %v", next, promote, wait)
	}
	if st := p.enter(1, now); st.Status != canaryAwaitingConfirm || st.Weight != 50 {
		t.Fatalf("manual step = %+v", st)
	}
	if _, promote, wait := p.afterStep(2); promote || !wait {
		t.Fatal("last step without auto_promote must wait for an operator")
	}
	p.AutoPromote = true
	if _, promote, wait := p.afterStep(2); !promote || wait {
		t.Fatal("last step with auto_promote must promote")
	}
}

func TestCanaryWeightProgressionAndRollback(t *testing.T) {
	ctx := context.Background()
	cs, tg := canaryFixture()
	stableImage, err := ensureCanaryWorkload(ctx, cs, tg)
	if err != nil {
		t.Fatal(err)
	}
	if stableImage != "reg/api:v1" {
		t.Fatalf("stable image = %q", stableImage)
	}
	dep, err := cs.AppsV1().Deployments("shop").Get(ctx, "api-canary", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *dep.Spec.Replicas != 1 || dep.Labels[labelManagedBy] != canaryManager || dep.Labels[labelCanaryID] != "7" {
		t.Fatalf("canary deployment meta = %+v replicas=%d", dep.Labels, *dep.Spec.Replicas)
	}
	pl := dep.Spec.Template.Labels
	if pl["app"] != "api-canary" || pl["tier"] != "web-canary" || pl["team"] != "a" {
		t.Fatalf("canary pod labels must not match stable selectors: %v", pl)
	}
	for k, v := range dep.Spec.Selector.MatchLabels {
		if pl[k] != v {
			t.Fatalf("canary selector %s=%s does not match its pods", k, v)
		}
	}
	images := map[string]string{}
	for _, c := range dep.Spec.Template.Spec.Containers {
		images[c.Name] = c.Image
	}
	if images["api"] != "reg/api:v2" || images["sidecar"] != "reg/proxy:1" {
		t.Fatalf("canary images = %v", images)
	}
	svc, err := cs.CoreV1().Services("shop").Get(ctx, "api-canary", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP || svc.Spec.Selector["app"] != "api-canary" ||
		len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].NodePort != 0 || svc.Spec.Ports[0].TargetPort.IntValue() != 8080 {
		t.Fatalf("canary service = %+v", svc.Spec)
	}

	if err := ensureCanaryIngress(ctx, cs, tg, 10); err != nil {
		t.Fatal(err)
	}
	ing, _ := cs.NetworkingV1().Ingresses("shop").Get(ctx, "web-canary", metav1.GetOptions{})
	if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != "nginx" || len(ing.Spec.TLS) != 0 {
		t.Fatalf("canary ingress class/tls = %+v", ing.Spec)
	}
	if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != "shop.example.com" ||
		len(ing.Spec.Rules[0].HTTP.Paths) != 1 || ing.Spec.Rules[0].HTTP.Paths[0].Path != "/api" ||
		ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name != "api-canary" {
		t.Fatalf("canary ingress rules = %+v", ing.Spec.Rules)
	}
	if _, ok := ing.Annotations["nginx.ingress.kubernetes.io/rewrite-target"]; ok {
		t.Fatal("unrelated stable annotations must not be copied")
	}
	if w := canaryWeightOf(t, cs, tg); w != "10" {
		t.Fatalf("weight = %s", w)
	}
	plan := canaryPlan{Steps: []canaryStep{{Weight: 10}, {Weight: 30}, {Weight: 50}, {Weight: 100}}}
	for i := 1; i < len(plan.Steps); i++ {
		if err := setCanaryWeight(ctx, cs, tg, plan.Steps[i].Weight); err != nil {
			t.Fatal(err)
		}
		if w := canaryWeightOf(t, cs, tg); w != map[int]string{1: "30", 2: "50", 3: "100"}[i] {
			t.Fatalf("step %d weight = %s", i+1, w)
		}
	}
	// Re-running ensure (retry) is idempotent and resets the weight it is given.
	if _, err := ensureCanaryWorkload(ctx, cs, tg); err != nil {
		t.Fatal(err)
	}
	if err := ensureCanaryIngress(ctx, cs, tg, 10); err != nil {
		t.Fatal(err)
	}
	if w := canaryWeightOf(t, cs, tg); w != "10" {
		t.Fatalf("weight after re-ensure = %s", w)
	}

	if err := deleteCanaryResources(ctx, cs, tg); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.NetworkingV1().Ingresses("shop").Get(ctx, "web-canary", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("canary ingress still present: %v", err)
	}
	if _, err := cs.CoreV1().Services("shop").Get(ctx, "api-canary", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("canary service still present: %v", err)
	}
	if _, err := cs.AppsV1().Deployments("shop").Get(ctx, "api-canary", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("canary deployment still present: %v", err)
	}
	st, _ := cs.AppsV1().Deployments("shop").Get(ctx, "api", metav1.GetOptions{})
	if st.Spec.Template.Spec.Containers[0].Image != "reg/api:v1" {
		t.Fatal("rollback must not touch the stable deployment")
	}
	if _, err := cs.NetworkingV1().Ingresses("shop").Get(ctx, "web", metav1.GetOptions{}); err != nil {
		t.Fatal("rollback must not touch the stable ingress")
	}
	// Deleting again is a no-op.
	if err := deleteCanaryResources(ctx, cs, tg); err != nil {
		t.Fatal(err)
	}
}

func TestCanaryPromote(t *testing.T) {
	ctx := context.Background()
	orig := canaryRolloutWait
	waited := []string{}
	canaryRolloutWait = func(_ context.Context, _ kubeIface, ns, name string, _ time.Duration, _ func(string, ...any)) error {
		waited = append(waited, ns+"/"+name)
		return nil
	}
	defer func() { canaryRolloutWait = orig }()
	cs, tg := canaryFixture()
	if _, err := ensureCanaryWorkload(ctx, cs, tg); err != nil {
		t.Fatal(err)
	}
	if err := ensureCanaryIngress(ctx, cs, tg, 50); err != nil {
		t.Fatal(err)
	}
	prev, err := promoteCanaryResources(ctx, cs, tg, time.Minute, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if prev != "reg/api:v1" {
		t.Fatalf("previous image = %q", prev)
	}
	if len(waited) != 1 || waited[0] != "shop/api" {
		t.Fatalf("waited for %v", waited)
	}
	st, _ := cs.AppsV1().Deployments("shop").Get(ctx, "api", metav1.GetOptions{})
	for _, c := range st.Spec.Template.Spec.Containers {
		if c.Name == "api" && c.Image != "reg/api:v2" {
			t.Fatalf("stable image after promote = %s", c.Image)
		}
		if c.Name == "sidecar" && c.Image != "reg/proxy:1" {
			t.Fatalf("sidecar changed: %s", c.Image)
		}
	}
	if _, err := cs.NetworkingV1().Ingresses("shop").Get(ctx, "web-canary", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("canary ingress must be removed after promote")
	}
	if _, err := cs.AppsV1().Deployments("shop").Get(ctx, "api-canary", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("canary deployment must be removed after promote")
	}
}

func TestCanaryPromoteRolloutFailureKeepsCanary(t *testing.T) {
	ctx := context.Background()
	orig := canaryRolloutWait
	canaryRolloutWait = func(context.Context, kubeIface, string, string, time.Duration, func(string, ...any)) error {
		return context.DeadlineExceeded
	}
	defer func() { canaryRolloutWait = orig }()
	cs, tg := canaryFixture()
	if _, err := ensureCanaryWorkload(ctx, cs, tg); err != nil {
		t.Fatal(err)
	}
	if err := ensureCanaryIngress(ctx, cs, tg, 50); err != nil {
		t.Fatal(err)
	}
	if _, err := promoteCanaryResources(ctx, cs, tg, time.Minute, func(string, ...any) {}); err == nil {
		t.Fatal("rollout failure must be reported")
	}
	if w := canaryWeightOf(t, cs, tg); w != "50" {
		t.Fatalf("canary must keep serving after a failed promote, weight=%s", w)
	}
}

func TestCanaryForeignObjectsAreNotTouched(t *testing.T) {
	ctx := context.Background()
	cs, tg := canaryFixture()
	foreign := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api-canary", Namespace: "shop"}}
	if _, err := cs.AppsV1().Deployments("shop").Create(ctx, foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureCanaryWorkload(ctx, cs, tg); err == nil || !strings.Contains(err.Error(), "不是本次金丝雀创建的") {
		t.Fatalf("foreign deployment must be refused, got %v", err)
	}
	other := tg
	other.ID = 8
	if err := deleteCanaryResources(ctx, cs, other); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.AppsV1().Deployments("shop").Get(ctx, "api-canary", metav1.GetOptions{}); err != nil {
		t.Fatal("foreign deployment must survive rollback")
	}
}

func TestCanaryIngressRequiresStableBackend(t *testing.T) {
	ctx := context.Background()
	cs, tg := canaryFixture()
	tg.Service = "missing"
	if err := ensureCanaryIngress(ctx, cs, tg, 10); err == nil {
		t.Fatal("ingress without a rule for the service must be refused")
	}
	cs2, tg2 := canaryFixture()
	ing, _ := cs2.NetworkingV1().Ingresses("shop").Get(ctx, "web", metav1.GetOptions{})
	ing.Annotations[annCanary] = "true"
	_, _ = cs2.NetworkingV1().Ingresses("shop").Update(ctx, ing, metav1.UpdateOptions{})
	if err := ensureCanaryIngress(ctx, cs2, tg2, 10); err == nil {
		t.Fatal("a canary ingress cannot be used as the stable ingress")
	}
}

func TestValidateCanaryInput(t *testing.T) {
	env := &EnvTarget{Name: "prod", TargetID: 1, Namespace: "shop", Deployment: "api", Container: "api"}
	in := &canaryIn{Env: "prod", Image: " reg/api:v2 ", Service: "api", Ingress: "web"}
	if err := validateCanaryInput(env, in); err != nil {
		t.Fatal(err)
	}
	if in.Image != "reg/api:v2" || len(in.Steps) != 4 || in.Replicas != 1 {
		t.Fatalf("normalised input = %+v", in)
	}
	cases := []struct {
		env *EnvTarget
		in  canaryIn
	}{
		{&EnvTarget{Name: "prod", Namespace: "shop", Helm: &HelmSpec{Release: "x"}}, canaryIn{Image: "a", Service: "api", Ingress: "web"}},
		{&EnvTarget{Name: "prod", Namespace: "shop", Deployment: "api"}, canaryIn{Image: "a", Service: "api", Ingress: "web"}},
		{env, canaryIn{Image: "a b", Service: "api", Ingress: "web"}},
		{env, canaryIn{Image: "a", Service: "Bad_Name", Ingress: "web"}},
		{env, canaryIn{Image: "a", Service: strings.Repeat("s", 60), Ingress: "web"}},
		{env, canaryIn{Image: "a", Service: "api", Ingress: "web", Steps: []canaryStep{{Weight: 50}, {Weight: 50}}}},
	}
	for i, c := range cases {
		in := c.in
		if err := validateCanaryInput(c.env, &in); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

func TestCanaryActionAllowed(t *testing.T) {
	if !canaryActionAllowed("rollback", canaryProgressing) || !canaryActionAllowed("rollback", canaryAwaitingApproval) {
		t.Fatal("rollback must be possible while running or awaiting approval")
	}
	if canaryActionAllowed("advance", canarySucceeded) || canaryActionAllowed("promote", canaryRolledBack) ||
		canaryActionAllowed("resume", canaryProgressing) || canaryActionAllowed("pause", canaryPromoting) {
		t.Fatal("finished or in-flight canaries must refuse forward actions")
	}
}

func TestCanaryLabelValue(t *testing.T) {
	if v := canaryLabelValue("api"); v != "api-canary" {
		t.Fatal(v)
	}
	long := canaryLabelValue(strings.Repeat("a", 63))
	if len(long) > 63 || !strings.HasSuffix(long, canarySuffix) {
		t.Fatalf("long label = %s (%d)", long, len(long))
	}
}

func TestCanaryMigration(t *testing.T) {
	migrations, err := store.ParseMigrations(New().Migrations())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.Version == 10 {
			if m.Name != "canary" || !strings.Contains(m.SQL, "canary_ingress") {
				t.Fatalf("0010 = %s", m.Name)
			}
			return
		}
	}
	t.Fatal("0010_canary migration is missing")
}
