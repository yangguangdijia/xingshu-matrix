package cicd

// Canary releases on top of ingress-nginx canary routing.
//
// For an application environment deployed by Deployment (not Helm) the engine
// clones the stable Deployment into <deployment>-canary running the new image,
// adds a <service>-canary Service selecting only the canary pods, and a
// <ingress>-canary Ingress that mirrors the stable Ingress rules pointing at
// the stable Service, annotated with
//   nginx.ingress.kubernetes.io/canary: "true"
//   nginx.ingress.kubernetes.io/canary-weight: "<weight>"
// Traffic then moves through the plan's steps (e.g. 10/30/50/100). A step can
// wait a fixed time (auto advance by the background worker) or wait for an
// operator (manual). Forward actions (start, advance, promote) pass the
// environment's change approval policy (approval type cicd.canary); pause and
// rollback never wait for approval. Promote patches the stable Deployment to
// the canary image, waits for its rollout, then removes the canary resources.
// Rollback sets the weight to 0 and removes the canary resources.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/approval"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

const (
	canaryApprovalType = "cicd.canary"
	annCanary          = "nginx.ingress.kubernetes.io/canary"
	annCanaryWeight    = "nginx.ingress.kubernetes.io/canary-weight"
	annIngressClass    = "kubernetes.io/ingress.class"
	labelManagedBy     = "app.kubernetes.io/managed-by"
	labelCanaryID      = "matrix.cicd/canary-id"
	canaryManager      = "matrix-cicd"
	canarySuffix       = "-canary"
	canaryOpTimeout    = 30 * time.Second
	canaryMaxAttempts  = 5
	canaryTick         = 5 * time.Second
)

// Canary statuses. Finished ones match the partial unique index of 0009.
const (
	canaryPending          = "pending"
	canaryAwaitingApproval = "awaiting_approval"
	canaryStarting         = "starting"
	canaryProgressing      = "progressing"
	canaryAwaitingConfirm  = "awaiting_confirm"
	canaryPaused           = "paused"
	canaryPromoting        = "promoting"
	canaryRollingBack      = "rolling_back"
	canarySucceeded        = "succeeded"
	canaryRolledBack       = "rolled_back"
	canaryFailed           = "failed"
	canaryRejected         = "rejected"
	canaryCancelled        = "cancelled"
)

// canaryRolloutWait waits for a Deployment rollout (replaced in tests).
var canaryRolloutWait = waitRollout

// ---------------------------------------------------------------- plan

type canaryStep struct {
	Weight   int  `json:"weight"`
	PauseSec int  `json:"pause_sec"`
	Manual   bool `json:"manual"`
}

type canaryPlan struct {
	Steps       []canaryStep `json:"steps"`
	Replicas    int32        `json:"replicas"`
	AutoPromote bool         `json:"auto_promote"`
}

func defaultCanarySteps() []canaryStep {
	return []canaryStep{{Weight: 10, PauseSec: 300}, {Weight: 30, PauseSec: 300}, {Weight: 50, Manual: true}, {Weight: 100, Manual: true}}
}

// normalize fills defaults and validates the plan.
func (p *canaryPlan) normalize() error {
	if len(p.Steps) == 0 {
		p.Steps = defaultCanarySteps()
	}
	if len(p.Steps) > 20 {
		return httpx.Validation("金丝雀步骤最多 20 步")
	}
	prev := 0
	for i, s := range p.Steps {
		if s.Weight < 1 || s.Weight > 100 {
			return httpx.Validation(fmt.Sprintf("第 %d 步权重须在 1-100 之间", i+1))
		}
		if s.Weight <= prev {
			return httpx.Validation(fmt.Sprintf("第 %d 步权重须大于上一步", i+1))
		}
		if s.PauseSec < 0 || s.PauseSec > 86400 {
			return httpx.Validation(fmt.Sprintf("第 %d 步观察时长须在 0-86400 秒之间", i+1))
		}
		prev = s.Weight
	}
	if p.Replicas == 0 {
		p.Replicas = 1
	}
	if p.Replicas < 1 || p.Replicas > 50 {
		return httpx.Validation("金丝雀副本数须在 1-50 之间")
	}
	return nil
}

// canaryState is the state a canary takes when it enters a step.
type canaryState struct {
	Status  string
	Step    int
	Weight  int
	NextAt  time.Time
	Message string
}

func (p canaryPlan) enter(i int, now time.Time) canaryState {
	s := p.Steps[i]
	st := canaryState{Step: i, Weight: s.Weight, NextAt: now}
	last := i == len(p.Steps)-1
	switch {
	case s.Manual && last:
		st.Status, st.Message = canaryAwaitingConfirm, fmt.Sprintf("第 %d 步（权重 %d%%）已生效，等待确认全量", i+1, s.Weight)
	case s.Manual:
		st.Status, st.Message = canaryAwaitingConfirm, fmt.Sprintf("第 %d 步（权重 %d%%）已生效，等待人工确认", i+1, s.Weight)
	default:
		st.Status, st.NextAt = canaryProgressing, now.Add(time.Duration(s.PauseSec)*time.Second)
		st.Message = fmt.Sprintf("第 %d 步（权重 %d%%）观察中", i+1, s.Weight)
	}
	return st
}

// afterStep decides what follows step i once its observation time elapsed:
// the next step, promotion (auto_promote) or waiting for an operator.
func (p canaryPlan) afterStep(i int) (next int, promote, wait bool) {
	if i+1 < len(p.Steps) {
		return i + 1, false, false
	}
	if p.AutoPromote {
		return -1, true, false
	}
	return -1, false, true
}

// ---------------------------------------------------------------- k8s

// canaryTarget names the stable and canary objects of one canary.
type canaryTarget struct {
	ID                                             int64
	Namespace, Deployment, Container               string
	Service, Ingress                               string
	CanaryDeployment, CanaryService, CanaryIngress string
	Image                                          string
	Replicas                                       int32
}

func canaryTargetOf(c *Canary) canaryTarget {
	return canaryTarget{ID: c.ID, Namespace: c.Namespace, Deployment: c.Deployment, Container: c.Container,
		Service: c.Service, Ingress: c.Ingress, CanaryDeployment: c.CanaryDeployment, CanaryService: c.CanaryService,
		CanaryIngress: c.CanaryIngress, Image: c.Image, Replicas: c.Plan.Replicas}
}

func kctx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, canaryOpTimeout)
}

// canaryLabelValue derives the canary pod label value from a stable one so
// stable selectors never match canary pods.
func canaryLabelValue(v string) string {
	if len(v) > 63-len(canarySuffix) {
		v = v[:63-len(canarySuffix)]
	}
	return strings.TrimRight(v, "-_.") + canarySuffix
}

func (t canaryTarget) meta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: t.Namespace, Labels: map[string]string{
		labelManagedBy: canaryManager, labelCanaryID: strconv.FormatInt(t.ID, 10)}}
}

// ownedBy reports whether an existing object was created by this canary.
func (t canaryTarget) ownedBy(m metav1.ObjectMeta) bool {
	return m.Labels[labelManagedBy] == canaryManager && m.Labels[labelCanaryID] == strconv.FormatInt(t.ID, 10)
}

func (t canaryTarget) foreign(kind, name string) error {
	return httpx.Conflict(fmt.Sprintf("%s %s/%s 已存在且不是本次金丝雀创建的，请改名或手动处理", kind, t.Namespace, name))
}

// ensureCanaryWorkload creates (or updates) the canary Deployment and Service
// and returns the stable image.
func ensureCanaryWorkload(ctx context.Context, cs kubeIface, t canaryTarget) (string, error) {
	c, cancel := kctx(ctx)
	defer cancel()
	stable, err := cs.AppsV1().Deployments(t.Namespace).Get(c, t.Deployment, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("读取主工作负载 %s/%s 失败: %w", t.Namespace, t.Deployment, err)
	}
	stableImage := ""
	for _, ct := range stable.Spec.Template.Spec.Containers {
		if ct.Name == t.Container {
			stableImage = ct.Image
		}
	}
	if stableImage == "" {
		return "", httpx.Conflict(fmt.Sprintf("主工作负载 %s 没有容器 %s", t.Deployment, t.Container))
	}
	svc, err := cs.CoreV1().Services(t.Namespace).Get(c, t.Service, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("读取主服务 %s/%s 失败: %w", t.Namespace, t.Service, err)
	}
	if len(svc.Spec.Selector) == 0 {
		return "", httpx.Conflict("主服务没有标签选择器，无法区分金丝雀实例")
	}
	// Every label used by the stable Service or Deployment selector gets a
	// canary value, so neither selects canary pods.
	rewrite := map[string]string{}
	for k, v := range svc.Spec.Selector {
		rewrite[k] = canaryLabelValue(v)
	}
	if stable.Spec.Selector != nil {
		for k, v := range stable.Spec.Selector.MatchLabels {
			rewrite[k] = canaryLabelValue(v)
		}
	}
	id := strconv.FormatInt(t.ID, 10)
	podLabels := map[string]string{}
	for k, v := range stable.Spec.Template.Labels {
		podLabels[k] = v
	}
	for k, v := range rewrite {
		podLabels[k] = v
	}
	podLabels[labelCanaryID] = id
	selector := map[string]string{labelCanaryID: id}
	svcSelector := map[string]string{labelCanaryID: id}
	for k := range svc.Spec.Selector {
		selector[k], svcSelector[k] = rewrite[k], rewrite[k]
	}

	tpl := *stable.Spec.Template.DeepCopy()
	tpl.Labels = podLabels
	for i := range tpl.Spec.Containers {
		if tpl.Spec.Containers[i].Name == t.Container {
			tpl.Spec.Containers[i].Image = t.Image
		}
	}
	replicas := t.Replicas
	if replicas < 1 {
		replicas = 1
	}
	dep := &appsv1.Deployment{ObjectMeta: t.meta(t.CanaryDeployment), Spec: appsv1.DeploymentSpec{
		Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: selector}, Template: tpl,
		Strategy: stable.Spec.Strategy, MinReadySeconds: stable.Spec.MinReadySeconds,
		ProgressDeadlineSeconds: stable.Spec.ProgressDeadlineSeconds,
	}}
	deps := cs.AppsV1().Deployments(t.Namespace)
	if cur, err := deps.Get(c, t.CanaryDeployment, metav1.GetOptions{}); err == nil {
		if !t.ownedBy(cur.ObjectMeta) {
			return "", t.foreign("工作负载", t.CanaryDeployment)
		}
		cur.Spec.Replicas, cur.Spec.Template = dep.Spec.Replicas, dep.Spec.Template
		if _, err := deps.Update(c, cur, metav1.UpdateOptions{FieldManager: canaryManager}); err != nil {
			return "", fmt.Errorf("更新金丝雀工作负载失败: %w", err)
		}
	} else if apierrors.IsNotFound(err) {
		if _, err := deps.Create(c, dep, metav1.CreateOptions{FieldManager: canaryManager}); err != nil {
			return "", fmt.Errorf("创建金丝雀工作负载失败: %w", err)
		}
	} else {
		return "", err
	}

	ports := make([]corev1.ServicePort, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		ports = append(ports, corev1.ServicePort{Name: p.Name, Protocol: p.Protocol, Port: p.Port,
			TargetPort: p.TargetPort, AppProtocol: p.AppProtocol})
	}
	csvc := &corev1.Service{ObjectMeta: t.meta(t.CanaryService), Spec: corev1.ServiceSpec{
		Type: corev1.ServiceTypeClusterIP, Selector: svcSelector, Ports: ports}}
	svcs := cs.CoreV1().Services(t.Namespace)
	if cur, err := svcs.Get(c, t.CanaryService, metav1.GetOptions{}); err == nil {
		if !t.ownedBy(cur.ObjectMeta) {
			return "", t.foreign("服务", t.CanaryService)
		}
		cur.Spec.Selector, cur.Spec.Ports = csvc.Spec.Selector, csvc.Spec.Ports
		if _, err := svcs.Update(c, cur, metav1.UpdateOptions{FieldManager: canaryManager}); err != nil {
			return "", fmt.Errorf("更新金丝雀服务失败: %w", err)
		}
	} else if apierrors.IsNotFound(err) {
		if _, err := svcs.Create(c, csvc, metav1.CreateOptions{FieldManager: canaryManager}); err != nil {
			return "", fmt.Errorf("创建金丝雀服务失败: %w", err)
		}
	} else {
		return "", err
	}
	return stableImage, nil
}

// canaryIngressSpec mirrors the stable Ingress rules whose backend is the
// stable Service, pointing them at the canary Service.
func canaryIngressSpec(stable *networkingv1.Ingress, t canaryTarget) (networkingv1.IngressSpec, error) {
	spec := networkingv1.IngressSpec{IngressClassName: stable.Spec.IngressClassName}
	for _, r := range stable.Spec.Rules {
		if r.HTTP == nil {
			continue
		}
		var paths []networkingv1.HTTPIngressPath
		for _, p := range r.HTTP.Paths {
			if p.Backend.Service == nil || p.Backend.Service.Name != t.Service {
				continue
			}
			cp := *p.DeepCopy()
			cp.Backend.Service.Name = t.CanaryService
			paths = append(paths, cp)
		}
		if len(paths) > 0 {
			spec.Rules = append(spec.Rules, networkingv1.IngressRule{Host: r.Host,
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: paths}}})
		}
	}
	if len(spec.Rules) == 0 {
		return spec, httpx.Conflict(fmt.Sprintf("主路由 %s 没有指向服务 %s 的规则", stable.Name, t.Service))
	}
	return spec, nil
}

// ensureCanaryIngress creates (or updates) the canary Ingress at weight.
func ensureCanaryIngress(ctx context.Context, cs kubeIface, t canaryTarget, weight int) error {
	c, cancel := kctx(ctx)
	defer cancel()
	ings := cs.NetworkingV1().Ingresses(t.Namespace)
	stable, err := ings.Get(c, t.Ingress, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("读取主路由 %s/%s 失败: %w", t.Namespace, t.Ingress, err)
	}
	if strings.EqualFold(stable.Annotations[annCanary], "true") {
		return httpx.Conflict("所选路由本身是金丝雀路由，请选择主路由")
	}
	spec, err := canaryIngressSpec(stable, t)
	if err != nil {
		return err
	}
	meta := t.meta(t.CanaryIngress)
	meta.Annotations = map[string]string{annCanary: "true", annCanaryWeight: strconv.Itoa(weight)}
	if v := stable.Annotations[annIngressClass]; v != "" {
		meta.Annotations[annIngressClass] = v
	}
	if cur, err := ings.Get(c, t.CanaryIngress, metav1.GetOptions{}); err == nil {
		if !t.ownedBy(cur.ObjectMeta) {
			return t.foreign("路由", t.CanaryIngress)
		}
		cur.Spec = spec
		if cur.Annotations == nil {
			cur.Annotations = map[string]string{}
		}
		for k, v := range meta.Annotations {
			cur.Annotations[k] = v
		}
		_, err = ings.Update(c, cur, metav1.UpdateOptions{FieldManager: canaryManager})
		return err
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	_, err = ings.Create(c, &networkingv1.Ingress{ObjectMeta: meta, Spec: spec}, metav1.CreateOptions{FieldManager: canaryManager})
	if err != nil {
		return fmt.Errorf("创建金丝雀路由失败: %w", err)
	}
	return nil
}

// setCanaryWeight changes the canary-weight annotation of the canary Ingress.
func setCanaryWeight(ctx context.Context, cs kubeIface, t canaryTarget, weight int) error {
	c, cancel := kctx(ctx)
	defer cancel()
	cur, err := cs.NetworkingV1().Ingresses(t.Namespace).Get(c, t.CanaryIngress, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("读取金丝雀路由失败: %w", err)
	}
	if !t.ownedBy(cur.ObjectMeta) {
		return t.foreign("路由", t.CanaryIngress)
	}
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:"true",%q:%q}}}`, annCanary, annCanaryWeight, strconv.Itoa(weight))
	_, err = cs.NetworkingV1().Ingresses(t.Namespace).Patch(c, t.CanaryIngress, types.MergePatchType, []byte(patch),
		metav1.PatchOptions{FieldManager: canaryManager})
	if err != nil {
		return fmt.Errorf("调整金丝雀权重失败: %w", err)
	}
	return nil
}

// deleteCanaryResources drops traffic first (weight 0, then the Ingress),
// then the Service and Deployment. Missing or foreign objects are skipped.
func deleteCanaryResources(ctx context.Context, cs kubeIface, t canaryTarget) error {
	c, cancel := kctx(ctx)
	defer cancel()
	var errs []error
	ings := cs.NetworkingV1().Ingresses(t.Namespace)
	if cur, err := ings.Get(c, t.CanaryIngress, metav1.GetOptions{}); err == nil && t.ownedBy(cur.ObjectMeta) {
		if err := setCanaryWeight(ctx, cs, t, 0); err != nil {
			errs = append(errs, err)
		}
		if err := ings.Delete(c, t.CanaryIngress, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("删除金丝雀路由失败: %w", err))
		}
	} else if err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, err)
	}
	svcs := cs.CoreV1().Services(t.Namespace)
	if cur, err := svcs.Get(c, t.CanaryService, metav1.GetOptions{}); err == nil && t.ownedBy(cur.ObjectMeta) {
		if err := svcs.Delete(c, t.CanaryService, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("删除金丝雀服务失败: %w", err))
		}
	} else if err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, err)
	}
	deps := cs.AppsV1().Deployments(t.Namespace)
	if cur, err := deps.Get(c, t.CanaryDeployment, metav1.GetOptions{}); err == nil && t.ownedBy(cur.ObjectMeta) {
		bg := metav1.DeletePropagationBackground
		if err := deps.Delete(c, t.CanaryDeployment, metav1.DeleteOptions{PropagationPolicy: &bg}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("删除金丝雀工作负载失败: %w", err))
		}
	} else if err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// promoteCanaryResources moves the stable Deployment to the canary image,
// waits for it, then removes the canary resources. It returns the image the
// stable Deployment ran before.
func promoteCanaryResources(ctx context.Context, cs kubeIface, t canaryTarget, timeout time.Duration, logf func(string, ...any)) (string, error) {
	c, cancel := kctx(ctx)
	prev, err := patchImage(c, cs, t.Namespace, t.Deployment, t.Container, t.Image)
	cancel()
	if err != nil {
		return prev, err
	}
	if err := canaryRolloutWait(ctx, cs, t.Namespace, t.Deployment, timeout, logf); err != nil {
		return prev, err
	}
	return prev, deleteCanaryResources(ctx, cs, t)
}

// ---------------------------------------------------------------- engine

func canaryNames(env *EnvTarget, service, ingress string) (dep, svc, ing string) {
	return env.Deployment + canarySuffix, service + canarySuffix, ingress + canarySuffix
}

func (m *Module) canaryEnv(ctx context.Context, c *Canary, pinned bool) (*EnvTarget, error) {
	a, err := m.loadApp(ctx, c.AppID)
	if err != nil {
		return nil, err
	}
	env, err := a.env(c.Env)
	if err != nil {
		return nil, err
	}
	if pinned && (env.TargetID != c.TargetID || envTargetFingerprint(env) != c.TargetConfig) {
		return nil, httpx.Conflict("应用环境的部署目标已变更，请回滚本次金丝雀后重新发起")
	}
	return env, nil
}

func rolloutTimeout(env *EnvTarget) time.Duration {
	if env != nil && env.RolloutTimeoutSec > 0 {
		return time.Duration(env.RolloutTimeoutSec) * time.Second
	}
	return 10 * time.Minute
}

func (m *Module) canaryLease(env *EnvTarget) time.Duration {
	return rolloutTimeout(env) + 5*time.Minute
}

func (m *Module) applyCanaryState(ctx context.Context, c *Canary, st canaryState) error {
	return m.updateCanary(ctx, c.ID, canaryUpdate{Status: &st.Status, Step: &st.Step, Weight: &st.Weight,
		NextAt: &st.NextAt, Message: &st.Message, StepStarted: true})
}

func (m *Module) canaryLogf(id int64) func(string, ...any) {
	return func(f string, x ...any) { m.log.Info("canary", "canary", id, "msg", fmt.Sprintf(f, x...)) }
}

// canaryStart creates the canary workload, waits for it, then opens step 1.
func (m *Module) canaryStart(ctx context.Context, c *Canary, actor string) error {
	env, err := m.canaryEnv(ctx, c, true)
	if err == nil {
		err = m.validateDeploymentTarget(ctx, env)
	}
	var conn *targetConn
	if err == nil {
		conn, err = m.targetClient(ctx, c.TargetID)
	}
	if err == nil {
		t := canaryTargetOf(c)
		var stableImage string
		if stableImage, err = ensureCanaryWorkload(ctx, conn.cs, t); err == nil {
			_ = m.updateCanary(ctx, c.ID, canaryUpdate{StableImage: &stableImage})
			m.addCanaryEvent(ctx, c.ID, "info", "已创建金丝雀工作负载与服务，等待实例就绪", actor, map[string]any{"stable_image": stableImage})
			if err = canaryRolloutWait(ctx, conn.cs, t.Namespace, t.CanaryDeployment, rolloutTimeout(env), m.canaryLogf(c.ID)); err == nil {
				err = ensureCanaryIngress(ctx, conn.cs, t, c.Plan.Steps[0].Weight)
			}
		}
	}
	if err != nil {
		msg := "启动失败：" + err.Error()
		if conn != nil {
			if cerr := deleteCanaryResources(context.Background(), conn.cs, canaryTargetOf(c)); cerr != nil {
				msg += "；清理金丝雀资源失败，请手动检查：" + cerr.Error()
			}
		}
		m.addCanaryEvent(ctx, c.ID, "error", msg, actor, nil)
		_ = m.updateCanary(context.Background(), c.ID, canaryUpdate{Status: canaryPtr(canaryFailed), Message: &msg,
			Weight: canaryPtr(0), Finished: true})
		return err
	}
	st := c.Plan.enter(0, time.Now())
	m.addCanaryEvent(ctx, c.ID, "step", st.Message, actor, map[string]any{"step": 1, "weight": st.Weight})
	return m.applyCanaryState(ctx, c, st)
}

// canaryAdvance moves to the next step.
func (m *Module) canaryAdvance(ctx context.Context, c *Canary, actor string) error {
	next := c.CurrentStep + 1
	if next >= len(c.Plan.Steps) {
		return httpx.Conflict("已是最后一步，请执行全量发布")
	}
	if _, err := m.canaryEnv(ctx, c, true); err != nil {
		return err
	}
	conn, err := m.targetClient(ctx, c.TargetID)
	if err != nil {
		return err
	}
	if err := setCanaryWeight(ctx, conn.cs, canaryTargetOf(c), c.Plan.Steps[next].Weight); err != nil {
		msg := "推进失败：" + err.Error()
		m.addCanaryEvent(ctx, c.ID, "error", msg, actor, nil)
		_ = m.updateCanary(ctx, c.ID, canaryUpdate{Message: &msg})
		return err
	}
	st := c.Plan.enter(next, time.Now())
	m.addCanaryEvent(ctx, c.ID, "step", st.Message, actor, map[string]any{"step": next + 1, "weight": st.Weight})
	return m.applyCanaryState(ctx, c, st)
}

// canaryPromote replaces the stable image and removes the canary.
func (m *Module) canaryPromote(ctx context.Context, c *Canary, actor string) error {
	env, err := m.canaryEnv(ctx, c, true)
	if err == nil {
		err = m.validateDeploymentTarget(ctx, env)
	}
	var conn *targetConn
	if err == nil {
		conn, err = m.targetClient(ctx, c.TargetID)
	}
	if err != nil {
		return m.canaryPromoteFailed(ctx, c, actor, err)
	}
	relID := int64(0)
	if c.ReleaseID != nil {
		relID = *c.ReleaseID
	} else if a, aerr := m.loadApp(ctx, c.AppID); aerr == nil {
		if relID, err = m.newRelease(ctx, a, env, c.Image, nil, "canary", actor); err == nil {
			_ = m.updateCanary(ctx, c.ID, canaryUpdate{ReleaseID: &relID})
		}
	}
	prev, err := promoteCanaryResources(ctx, conn.cs, canaryTargetOf(c), rolloutTimeout(env), m.canaryLogf(c.ID))
	if relID > 0 {
		status, msg := "succeeded", "canary promoted"
		if err != nil {
			status, msg = "failed", err.Error()
		}
		if prev == "" {
			prev = c.StableImage
		}
		_, _ = m.db.Exec(context.Background(), `UPDATE cicd_releases SET status=$2, message=$3, previous_image=$4, finished_at=now() WHERE id=$1`,
			relID, status, truncRunes(msg, 1000), prev)
	}
	if err != nil {
		return m.canaryPromoteFailed(ctx, c, actor, err)
	}
	msg := "已全量发布：主工作负载已切换到新镜像，金丝雀资源已清理"
	m.addCanaryEvent(ctx, c.ID, "step", msg, actor, map[string]any{"image": c.Image, "previous_image": prev})
	return m.updateCanary(ctx, c.ID, canaryUpdate{Status: canaryPtr(canarySucceeded), Weight: canaryPtr(100),
		Message: &msg, Finished: true})
}

// canaryPromoteFailed keeps the canary open so the operator can retry or roll back.
func (m *Module) canaryPromoteFailed(ctx context.Context, c *Canary, actor string, err error) error {
	msg := "全量发布失败，可重试或回滚：" + err.Error()
	m.addCanaryEvent(ctx, c.ID, "error", msg, actor, nil)
	_ = m.updateCanary(context.Background(), c.ID, canaryUpdate{Status: canaryPtr(canaryAwaitingConfirm), Message: &msg})
	return err
}

// canaryRollback sets the weight to 0 and deletes the canary resources.
func (m *Module) canaryRollback(ctx context.Context, c *Canary, actor string) error {
	conn, err := m.targetClient(ctx, c.TargetID)
	if err == nil {
		err = deleteCanaryResources(ctx, conn.cs, canaryTargetOf(c))
	}
	if err != nil {
		msg := "回滚未完成，将自动重试：" + err.Error()
		status := canaryRollingBack
		finished := false
		if c.Attempts >= canaryMaxAttempts {
			msg, status, finished = "回滚多次失败，请手动清理金丝雀资源："+err.Error(), canaryFailed, true
		}
		m.addCanaryEvent(ctx, c.ID, "error", msg, actor, nil)
		retryAt := time.Now().Add(time.Duration(c.Attempts) * 30 * time.Second)
		_ = m.updateCanary(context.Background(), c.ID, canaryUpdate{Status: &status, Message: &msg, Finished: finished, NextAt: &retryAt})
		return err
	}
	msg := "已回滚：金丝雀流量归零，金丝雀资源已删除"
	m.addCanaryEvent(ctx, c.ID, "step", msg, actor, nil)
	return m.updateCanary(ctx, c.ID, canaryUpdate{Status: canaryPtr(canaryRolledBack), Weight: canaryPtr(0),
		Message: &msg, Finished: true, ApprovalID: canaryPtr(int64(0)), PendingAction: canaryPtr("")})
}

func newCanaryOwner(kind string) string { return fmt.Sprintf("%s-%d", kind, time.Now().UnixNano()) }

// runCanaryOp claims the lease, sets the in-flight status and runs op (in the
// background when async). The lease is released when op returns.
func (m *Module) runCanaryOp(ctx context.Context, id int64, inflight string, async bool, actor string,
	op func(context.Context, *Canary, string) error) (*Canary, error) {
	c, err := m.loadCanary(ctx, id)
	if err != nil {
		return nil, err
	}
	env, _ := m.canaryEnv(ctx, c, false)
	owner := newCanaryOwner("api")
	ok, err := m.claimCanary(ctx, id, owner, m.canaryLease(env))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, httpx.Conflict("该金丝雀正在执行其他操作，请稍后再试")
	}
	if c, err = m.loadCanary(ctx, id); err != nil {
		m.releaseCanary(id, owner)
		return nil, err
	}
	if inflight != "" {
		_ = m.updateCanary(ctx, id, canaryUpdate{Status: &inflight, ResetAttempts: true})
		c.Status = inflight
	}
	if !async {
		defer m.releaseCanary(id, owner)
		if err := op(ctx, c, actor); err != nil {
			return nil, err
		}
		return m.loadCanary(ctx, id)
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.releaseCanary(id, owner)
		bg := context.Background()
		err := op(bg, c, actor)
		m.recordCanaryAudit(bg, c, "cicd.canary."+inflight+".done", 0, actor, err)
		if err != nil {
			m.log.Warn("canary operation failed", "canary", id, "status", inflight, "err", err)
		}
	}()
	return c, nil
}

// recordCanaryAudit writes an audit entry for actions not driven by an HTTP
// request (background completion, worker steps, approved tickets).
func (m *Module) recordCanaryAudit(ctx context.Context, c *Canary, action string, userID int64, user string, err error) {
	if m.d == nil || m.d.Audit == nil || c == nil {
		return
	}
	detail := []byte(fmt.Sprintf(`{"app_id":%d,"env":%q,"image":%q,"step":%d}`, c.AppID, c.Env, c.Image, c.CurrentStep+1))
	e := audit.Entry{Username: user, Method: "SYSTEM", Resource: "cicd_canary", ResourceID: strconv.FormatInt(c.ID, 10),
		Action: action, StatusCode: http.StatusOK, Result: audit.ResultSuccess, Detail: detail, CreatedAt: time.Now()}
	if userID > 0 {
		e.UserID = &userID
	}
	if err != nil {
		e.StatusCode, e.Result = http.StatusInternalServerError, audit.ResultFailure
		var he *httpx.Error
		if errors.As(err, &he) {
			e.StatusCode = he.Status
		}
	}
	m.d.Audit.Record(ctx, e)
}

// canaryLoop advances due canaries and resumes operations interrupted by a restart.
func (m *Module) canaryLoop(ctx context.Context) {
	t := time.NewTicker(canaryTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ids, err := m.dueCanaries(ctx)
		if err != nil {
			continue
		}
		for _, id := range ids {
			m.canaryWork(ctx, id)
		}
	}
}

func (m *Module) canaryWork(ctx context.Context, id int64) {
	c, err := m.loadCanary(ctx, id)
	if err != nil {
		return
	}
	env, _ := m.canaryEnv(ctx, c, false)
	owner := newCanaryOwner("worker")
	if ok, err := m.claimCanary(ctx, id, owner, m.canaryLease(env)); err != nil || !ok {
		return
	}
	defer m.releaseCanary(id, owner)
	if c, err = m.loadCanary(ctx, id); err != nil {
		return
	}
	const actor = "system"
	action := ""
	switch c.Status {
	case canaryProgressing:
		if c.NextAt.After(time.Now()) {
			return
		}
		_, promote, wait := c.Plan.afterStep(c.CurrentStep)
		switch {
		case wait:
			msg := "全部步骤已完成，等待确认全量"
			m.addCanaryEvent(ctx, id, "info", msg, actor, nil)
			_ = m.updateCanary(ctx, id, canaryUpdate{Status: canaryPtr(canaryAwaitingConfirm), Message: &msg})
			return
		case promote:
			action = "promote"
			_ = m.updateCanary(ctx, id, canaryUpdate{Status: canaryPtr(canaryPromoting)})
			err = m.canaryPromote(ctx, c, actor)
		default:
			action = "advance"
			if err = m.canaryAdvance(ctx, c, actor); err != nil {
				// Stop auto progression; the operator decides.
				msg := "自动推进失败，已暂停：" + err.Error()
				_ = m.updateCanary(ctx, id, canaryUpdate{Status: canaryPtr(canaryPaused), Message: &msg})
			}
		}
	case canaryStarting:
		// The process that was starting it is gone; clean up rather than guess.
		action = "start"
		err = errors.New("服务重启导致启动中断")
		if conn, cerr := m.targetClient(ctx, c.TargetID); cerr == nil {
			_ = deleteCanaryResources(ctx, conn.cs, canaryTargetOf(c))
		}
		msg := "启动中断（服务重启），已清理金丝雀资源，请重新发起"
		m.addCanaryEvent(ctx, id, "error", msg, actor, nil)
		_ = m.updateCanary(ctx, id, canaryUpdate{Status: canaryPtr(canaryFailed), Message: &msg, Weight: canaryPtr(0), Finished: true})
	case canaryPromoting:
		action = "promote"
		err = m.canaryPromote(ctx, c, actor)
	case canaryRollingBack:
		action = "rollback"
		err = m.canaryRollback(ctx, c, actor)
	default:
		return
	}
	m.recordCanaryAudit(ctx, c, "cicd.canary."+action, 0, actor, err)
}

// ---------------------------------------------------------------- approval

// canaryPayload is the approval ticket payload of type cicd.canary.
type canaryPayload struct {
	Action       string `json:"action"` // start|advance|promote
	CanaryID     int64  `json:"canary_id"`
	AppID        int64  `json:"app_id"`
	App          string `json:"app"`
	Env          string `json:"env"`
	TargetID     int64  `json:"target_id"`
	TargetConfig string `json:"target_config"`
	Image        string `json:"image"`
	Step         int    `json:"step"`
}

type canaryHandler struct{ m *Module }

func canaryOpFor(m *Module, action string) (inflight string, async bool, op func(context.Context, *Canary, string) error) {
	switch action {
	case "start":
		return canaryStarting, true, m.canaryStart
	case "advance":
		return "", false, m.canaryAdvance
	case "promote":
		return canaryPromoting, true, m.canaryPromote
	}
	return "", false, nil
}

func (h *canaryHandler) OnApproved(ctx context.Context, t *approval.Ticket) (any, error) {
	var p canaryPayload
	if err := t.DecodePayload(&p); err != nil {
		return nil, err
	}
	m := h.m
	c, err := m.loadCanary(ctx, p.CanaryID)
	if err != nil {
		return nil, err
	}
	if c.ApprovalID == nil || *c.ApprovalID != t.ID || c.PendingAction != p.Action || c.AppID != p.AppID ||
		c.Env != p.Env || c.Env != t.Env || c.Image != p.Image || c.CurrentStep != p.Step {
		return nil, httpx.Conflict("金丝雀状态已变化，审批单已失效")
	}
	if c.TargetID != p.TargetID || c.TargetConfig != p.TargetConfig {
		return nil, httpx.Conflict("金丝雀部署目标已变化，审批单已失效")
	}
	if _, err := m.canaryEnv(ctx, c, true); err != nil {
		return nil, err
	}
	if err := (&deployHandler{m}).reauthorize(ctx, t, c.Env); err != nil {
		return nil, err
	}
	_ = m.updateCanary(ctx, c.ID, canaryUpdate{ApprovalID: canaryPtr(int64(0)), PendingAction: canaryPtr("")})
	m.addCanaryEvent(ctx, c.ID, "operator", fmt.Sprintf("审批单 #%d 已通过（%s）", t.ID, canaryActionName(p.Action)), t.ApproverName, nil)
	inflight, async, op := canaryOpFor(m, p.Action)
	if op == nil {
		return nil, httpx.Validation("unknown canary action")
	}
	out, err := m.runCanaryOp(ctx, c.ID, inflight, async, t.ApplicantName, op)
	if !async {
		m.recordCanaryAudit(ctx, c, "cicd.canary."+p.Action, t.ApplicantID, t.ApplicantName, err)
	}
	if err != nil {
		return nil, err
	}
	return gin.H{"canary_id": out.ID, "status": out.Status}, nil
}

func (h *canaryHandler) OnRejected(ctx context.Context, t *approval.Ticket) error {
	var p canaryPayload
	if err := t.DecodePayload(&p); err != nil {
		return err
	}
	m := h.m
	c, err := m.loadCanary(ctx, p.CanaryID)
	if err != nil || c.ApprovalID == nil || *c.ApprovalID != t.ID {
		return nil
	}
	msg := fmt.Sprintf("审批单 #%d 未通过（%s）", t.ID, canaryActionName(p.Action))
	m.addCanaryEvent(ctx, c.ID, "operator", msg, t.ApproverName, nil)
	u := canaryUpdate{ApprovalID: canaryPtr(int64(0)), PendingAction: canaryPtr(""), Message: &msg}
	if p.Action == "start" && c.Status == canaryAwaitingApproval {
		u.Status, u.Finished = canaryPtr(canaryRejected), true
	}
	return m.updateCanary(ctx, c.ID, u)
}

func canaryActionName(a string) string {
	return map[string]string{"start": "发起金丝雀", "advance": "推进下一步", "promote": "全量发布"}[a]
}

// ---------------------------------------------------------------- HTTP

func (m *Module) registerCanary(g gin.IRoutes, read, deploy gin.HandlerFunc) {
	if m.d != nil && m.d.Approval != nil {
		if err := m.d.Approval.Register(approval.TypeSpec{
			Type: canaryApprovalType, Module: "cicd", Description: "金丝雀发布（发起 / 推进 / 全量）",
			CreatePermission: "cicd:deploy", Timeout: 30 * time.Minute, Handler: &canaryHandler{m},
		}); err != nil {
			m.log.Error("register canary approval type", "err", err)
		}
	}
	g.GET("/applications/:id/canaries", read, m.listCanaries)
	g.POST("/applications/:id/canaries", deploy, m.createCanary)
	g.GET("/canaries/:id", read, m.getCanary)
	g.POST("/canaries/:id/advance", deploy, m.canaryAction("advance"))
	g.POST("/canaries/:id/promote", deploy, m.canaryAction("promote"))
	g.POST("/canaries/:id/pause", deploy, m.canaryAction("pause"))
	g.POST("/canaries/:id/resume", deploy, m.canaryAction("resume"))
	g.POST("/canaries/:id/rollback", deploy, m.canaryAction("rollback"))
}

func (m *Module) listCanaries(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	rows, err := m.listCanaryRows(c, id, c.Query("env"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, rows)
}

func (m *Module) getCanary(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	cn, err := m.loadCanary(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if cn.Events, err = m.canaryEvents(c, id); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, cn)
}

type canaryIn struct {
	Env         string       `json:"env" binding:"required,max=64"`
	Image       string       `json:"image" binding:"required,max=512"`
	Service     string       `json:"service" binding:"required,max=63"`
	Ingress     string       `json:"ingress" binding:"required,max=253"`
	Steps       []canaryStep `json:"steps"`
	Replicas    int32        `json:"replicas"`
	AutoPromote bool         `json:"auto_promote"`
	Reason      string       `json:"reason" binding:"max=2000"`
}

// validateCanaryInput checks names and image of a new canary on env.
func validateCanaryInput(env *EnvTarget, in *canaryIn) error {
	if env.Helm != nil {
		return httpx.Conflict("Helm 部署的环境暂不支持金丝雀发布")
	}
	if env.Deployment == "" || env.Container == "" || env.Namespace == "" {
		return httpx.Conflict("应用环境未配置工作负载、容器或命名空间")
	}
	in.Image = strings.TrimSpace(in.Image)
	if in.Image == "" || strings.ContainsAny(in.Image, " \t\r\n") {
		return httpx.Validation("镜像地址不合法")
	}
	if errs := validation.IsDNS1035Label(in.Service); len(errs) > 0 {
		return httpx.Validation("服务名称不合法")
	}
	if errs := validation.IsDNS1123Subdomain(in.Ingress); len(errs) > 0 {
		return httpx.Validation("路由名称不合法")
	}
	dep, svc, ing := canaryNames(env, in.Service, in.Ingress)
	if len(validation.IsDNS1123Subdomain(dep)) > 0 {
		return httpx.Validation("工作负载名称过长，无法生成金丝雀名称")
	}
	if len(validation.IsDNS1035Label(svc)) > 0 {
		return httpx.Validation("服务名称过长，无法生成金丝雀服务名称（上限 56 个字符）")
	}
	if len(validation.IsDNS1123Subdomain(ing)) > 0 {
		return httpx.Validation("路由名称过长，无法生成金丝雀路由名称")
	}
	plan := canaryPlan{Steps: in.Steps, Replicas: in.Replicas, AutoPromote: in.AutoPromote}
	if err := plan.normalize(); err != nil {
		return err
	}
	in.Steps, in.Replicas = plan.Steps, plan.Replicas
	return nil
}

func (m *Module) createCanary(c *gin.Context) {
	id, ok := httpx.ParamID(c, "id")
	if !ok {
		return
	}
	var in canaryIn
	if !httpx.Bind(c, &in) {
		return
	}
	a, err := m.loadApp(c, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	env, err := a.env(in.Env)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	p := rbac.PrincipalFrom(c)
	if !p.CanEnv(env.Name) {
		httpx.Fail(c, httpx.Forbidden("environment not in your scope: "+env.Name))
		return
	}
	if err := validateCanaryInput(env, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	if err := m.validateDeploymentTarget(c, env); err != nil {
		httpx.Fail(c, err)
		return
	}
	fp := envTargetFingerprint(env)
	if fp == "" {
		httpx.Fail(c, httpx.Conflict("deployment destination could not be pinned"))
		return
	}
	if m.d.Approval == nil {
		httpx.Fail(c, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "审批服务不可用，金丝雀发布已拒绝"))
		return
	}
	dep, svc, ing := canaryNames(env, in.Service, in.Ingress)
	cn := &Canary{AppID: a.ID, Env: env.Name, TargetID: env.TargetID, TargetConfig: fp, Image: in.Image,
		Namespace: env.Namespace, Deployment: env.Deployment, Container: env.Container, Service: in.Service,
		Ingress: in.Ingress, CanaryDeployment: dep, CanaryService: svc, CanaryIngress: ing,
		Plan:   canaryPlan{Steps: in.Steps, Replicas: in.Replicas, AutoPromote: in.AutoPromote},
		Status: canaryPending, Reason: in.Reason, CreatedBy: p.Username, CreatedByID: p.UserID, CurrentStep: -1}
	if err := m.insertCanary(c, cn); err != nil {
		httpx.Fail(c, err)
		return
	}
	audit.SetResource(c, "cicd_canary", strconv.FormatInt(cn.ID, 10))
	audit.SetAction(c, "cicd.canary.create")
	audit.SetDetail(c, gin.H{"app_id": a.ID, "env": env.Name, "image": in.Image, "steps": in.Steps})
	m.addCanaryEvent(c, cn.ID, "operator", "发起金丝雀发布", p.Username, gin.H{"image": in.Image, "steps": in.Steps})
	pending, err := m.canaryGate(c, p, a, cn, "start")
	if err != nil {
		msg := "发起失败：" + err.Error()
		_ = m.updateCanary(c, cn.ID, canaryUpdate{Status: canaryPtr(canaryCancelled), Message: &msg, Finished: true})
		httpx.Fail(c, err)
		return
	}
	if pending {
		return
	}
	out, err := m.runCanaryOp(c, cn.ID, canaryStarting, true, p.Username, m.canaryStart)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, out)
}

// canaryGate passes a forward action through the environment approval policy.
// When a ticket is opened it records it on the canary and writes the 202 response.
func (m *Module) canaryGate(c *gin.Context, p *rbac.Principal, a *Application, cn *Canary, action string) (bool, error) {
	if m.d.Approval == nil {
		return false, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "审批服务不可用，金丝雀操作已拒绝")
	}
	title := fmt.Sprintf("金丝雀%s %s → %s (%s)", canaryActionName(action), a.Name, cn.Env, cn.Image)
	if action == "advance" && cn.CurrentStep+1 < len(cn.Plan.Steps) {
		title = fmt.Sprintf("金丝雀推进至 %d%% %s → %s (%s)", cn.Plan.Steps[cn.CurrentStep+1].Weight, a.Name, cn.Env, cn.Image)
	}
	t, pending, err := m.d.Approval.Gate(c, p, approval.CreateInput{
		Type: canaryApprovalType, Title: title, Env: cn.Env, Reason: cn.Reason,
		Payload: canaryPayload{Action: action, CanaryID: cn.ID, AppID: a.ID, App: a.Name, Env: cn.Env,
			TargetID: cn.TargetID, TargetConfig: cn.TargetConfig, Image: cn.Image, Step: cn.CurrentStep},
	})
	if err != nil || !pending {
		return false, err
	}
	u := canaryUpdate{ApprovalID: &t.ID, PendingAction: &action,
		Message: canaryPtr(fmt.Sprintf("等待审批（%s）", canaryActionName(action)))}
	if action == "start" {
		u.Status = canaryPtr(canaryAwaitingApproval)
	}
	if err := m.updateCanary(c, cn.ID, u); err != nil {
		return false, err
	}
	m.addCanaryEvent(c, cn.ID, "operator", fmt.Sprintf("已提交审批单 #%d（%s）", t.ID, canaryActionName(action)), p.Username, nil)
	approval.RespondPending(c, t)
	return true, nil
}

// canaryAllowed lists the statuses each operator action accepts.
var canaryAllowed = map[string][]string{
	"advance":  {canaryProgressing, canaryAwaitingConfirm, canaryPaused},
	"promote":  {canaryProgressing, canaryAwaitingConfirm, canaryPaused},
	"pause":    {canaryProgressing, canaryAwaitingConfirm},
	"resume":   {canaryPaused},
	"rollback": {canaryPending, canaryAwaitingApproval, canaryProgressing, canaryAwaitingConfirm, canaryPaused, canaryRollingBack},
}

func canaryActionAllowed(action, status string) bool {
	for _, s := range canaryAllowed[action] {
		if s == status {
			return true
		}
	}
	return false
}

type canaryActionIn struct {
	Reason string `json:"reason" binding:"max=2000"`
}

func (m *Module) canaryAction(action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := httpx.ParamID(c, "id")
		if !ok {
			return
		}
		var in canaryActionIn
		if c.Request.ContentLength > 0 && !httpx.Bind(c, &in) {
			return
		}
		cn, err := m.loadCanary(c, id)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		p := rbac.PrincipalFrom(c)
		if !p.CanEnv(cn.Env) {
			httpx.Fail(c, httpx.Forbidden("environment not in your scope: "+cn.Env))
			return
		}
		audit.SetResource(c, "cicd_canary", strconv.FormatInt(cn.ID, 10))
		audit.SetAction(c, "cicd.canary."+action)
		audit.SetDetail(c, gin.H{"app_id": cn.AppID, "env": cn.Env, "image": cn.Image, "step": cn.CurrentStep + 1,
			"weight": cn.Weight, "reason": in.Reason})
		if !canaryActionAllowed(action, cn.Status) {
			httpx.Fail(c, httpx.Conflict("当前状态不允许该操作"))
			return
		}
		if cn.PendingAction != "" && action != "rollback" && action != "pause" {
			httpx.Fail(c, httpx.Conflict("已有待审批的金丝雀操作，请等待审批结果"))
			return
		}
		m.addCanaryEvent(c, cn.ID, "operator", canaryOperatorText(action, in.Reason), p.Username, nil)
		var out *Canary
		switch action {
		case "pause":
			out, err = m.runCanaryOp(c, id, "", false, p.Username, func(ctx context.Context, cn *Canary, _ string) error {
				return m.updateCanary(ctx, cn.ID, canaryUpdate{Status: canaryPtr(canaryPaused),
					Message: canaryPtr(fmt.Sprintf("已暂停（当前权重 %d%%）", cn.Weight))})
			})
		case "resume":
			out, err = m.runCanaryOp(c, id, "", false, p.Username, func(ctx context.Context, cn *Canary, _ string) error {
				st := cn.Plan.enter(cn.CurrentStep, time.Now())
				return m.applyCanaryState(ctx, cn, st)
			})
		case "rollback":
			if cn.Status == canaryPending || cn.Status == canaryAwaitingApproval {
				// Nothing was created yet.
				msg := "已取消"
				err = m.updateCanary(c, cn.ID, canaryUpdate{Status: canaryPtr(canaryCancelled), Message: &msg, Finished: true,
					ApprovalID: canaryPtr(int64(0)), PendingAction: canaryPtr("")})
				if err == nil {
					out, err = m.loadCanary(c, cn.ID)
				}
				break
			}
			out, err = m.runCanaryOp(c, id, canaryRollingBack, true, p.Username, m.canaryRollback)
		default: // advance, promote
			if action == "advance" && cn.CurrentStep+1 >= len(cn.Plan.Steps) {
				httpx.Fail(c, httpx.Conflict("已是最后一步，请执行全量发布"))
				return
			}
			a, aerr := m.loadApp(c, cn.AppID)
			if aerr != nil {
				httpx.Fail(c, aerr)
				return
			}
			if _, err := m.canaryEnv(c, cn, true); err != nil {
				httpx.Fail(c, err)
				return
			}
			pending, gerr := m.canaryGate(c, p, a, cn, action)
			if gerr != nil {
				httpx.Fail(c, gerr)
				return
			}
			if pending {
				return
			}
			inflight, async, op := canaryOpFor(m, action)
			out, err = m.runCanaryOp(c, id, inflight, async, p.Username, op)
		}
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, out)
	}
}

func canaryOperatorText(action, reason string) string {
	s := map[string]string{"advance": "操作：推进下一步", "promote": "操作：全量发布", "pause": "操作：暂停",
		"resume": "操作：继续", "rollback": "操作：回滚"}[action]
	if reason = strings.TrimSpace(reason); reason != "" {
		s += "，原因：" + truncRunes(reason, 200)
	}
	return s
}
