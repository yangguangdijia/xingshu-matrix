package cicd

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestVerifyWebhook(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	h := http.Header{}
	h.Set("X-Gitlab-Token", "s3cret")
	if p, err := verifyWebhook(h, body, "s3cret"); err != nil || p != "gitlab" {
		t.Fatalf("gitlab: %v %v", p, err)
	}
	if _, err := verifyWebhook(h, body, "other"); err == nil {
		t.Fatal("gitlab bad token accepted")
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	g := http.Header{}
	g.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	if p, err := verifyWebhook(g, body, "s3cret"); err != nil || p != "github" {
		t.Fatalf("github: %v %v", p, err)
	}
	if _, err := verifyWebhook(g, append(body, ' '), "s3cret"); err == nil {
		t.Fatal("tampered body accepted")
	}
	if _, err := verifyWebhook(http.Header{}, body, "s3cret"); err == nil {
		t.Fatal("unsigned accepted")
	}
}

func TestJenkinsTriggerAndFollow(t *testing.T) {
	var srv *httptest.Server
	polls := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "bot" || p != "tok" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/crumbIssuer/api/json":
			http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "x", Path: "/"})
			w.Write([]byte(`{"crumb":"abc","crumbRequestField":"Jenkins-Crumb"}`))
		case r.URL.Path == "/job/team/job/app/buildWithParameters":
			if r.Header.Get("Jenkins-Crumb") != "abc" || r.FormValue("TAG") != "v1" {
				w.WriteHeader(403)
				return
			}
			if c, _ := r.Cookie("JSESSIONID"); c == nil {
				w.WriteHeader(403)
				return
			}
			w.Header().Set("Location", srv.URL+"/queue/item/7/")
			w.WriteHeader(201)
		case r.URL.Path == "/queue/item/7/api/json":
			w.Write([]byte(`{"executable":{"url":"` + srv.URL + `/job/team/job/app/3/"}}`))
		case r.URL.Path == "/job/team/job/app/3/logText/progressiveText":
			polls++
			if r.URL.Query().Get("start") == "0" {
				w.Header().Set("X-Text-Size", "6")
				w.Header().Set("X-More-Data", "true")
				w.Write([]byte("hello\n"))
				return
			}
			w.Header().Set("X-Text-Size", "12")
			w.Write([]byte("world\n"))
		case r.URL.Path == "/job/team/job/app/3/api/json":
			w.Write([]byte(`{"building":false,"result":"SUCCESS"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	j := newJenkins(srv.URL, "bot", "tok")
	ctx := context.Background()
	q, err := j.trigger(ctx, "team/app", map[string]string{"TAG": "v1"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := j.waitExecutable(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	res, err := j.follow(ctx, b, func(f string, a ...any) { lines = append(lines, a[0].(string)) })
	if err != nil || res != "SUCCESS" || strings.Join(lines, ",") != "hello,world" {
		t.Fatalf("res=%q err=%v lines=%v", res, err, lines)
	}
}

func TestPatchImageAndRollout(t *testing.T) {
	one := int32(1)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod", Generation: 1},
		Spec: appsv1.DeploymentSpec{Replicas: &one, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "reg/web:v1"}, {Name: "sidecar", Image: "envoy:1"}}}}},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	cs := fake.NewSimpleClientset(d)
	prev, err := patchImage(context.Background(), cs, "prod", "web", "app", "reg/web:v2")
	if err != nil || prev != "reg/web:v1" {
		t.Fatalf("prev=%q err=%v", prev, err)
	}
	got, _ := cs.AppsV1().Deployments("prod").Get(context.Background(), "web", metav1.GetOptions{})
	if got.Spec.Template.Spec.Containers[0].Image != "reg/web:v2" || got.Spec.Template.Spec.Containers[1].Image != "envoy:1" {
		t.Fatalf("containers after patch: %+v", got.Spec.Template.Spec.Containers)
	}
	if err := waitRollout(context.Background(), cs, "prod", "web", 5*time.Second, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := patchImage(context.Background(), cs, "prod", "web", "nope", "x"); err == nil {
		t.Fatal("missing container accepted")
	}
	got.Status.UpdatedReplicas = 0
	if done, _, _ := rolloutStatus(got); done {
		t.Fatal("rollout should be incomplete")
	}
	got.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Reason: "ProgressDeadlineExceeded"}}
	if _, _, err := rolloutStatus(got); err == nil {
		t.Fatal("deadline exceeded not reported")
	}
}

func TestCollectApplicationInventoryUsesWorkloadSelector(t *testing.T) {
	replicas := int32(2)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "checkout"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "checkout"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "registry/checkout:v2"}}}},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1, ObservedGeneration: 3},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-abc", Namespace: "shop", Labels: map[string]string{"app.kubernetes.io/name": "checkout"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "checkout-abc", Controller: ptr(true)}}},
		Spec:   corev1.PodSpec{NodeName: "worker-1", Containers: []corev1.Container{{Name: "app", Image: "registry/checkout:v2"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.8", ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true, RestartCount: 2, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}},
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app.kubernetes.io/name": "checkout"}, Ports: []corev1.ServicePort{{Port: 8080}}}}
	unrelated := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "billing", Namespace: "shop"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app.kubernetes.io/name": "billing"}}}
	cs := fake.NewSimpleClientset(deployment, pod, service, unrelated)

	got, err := collectApplicationInventory(context.Background(), cs, "checkout", EnvTarget{Name: "prod", Namespace: "shop", Deployment: "checkout"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Workloads) != 1 || got.Workloads[0].Name != "checkout" || got.Workloads[0].Ready != 1 {
		t.Fatalf("unexpected workloads: %+v", got.Workloads)
	}
	if len(got.Pods) != 1 || got.Pods[0].Name != "checkout-abc" || got.Pods[0].Restarts != 2 || got.Pods[0].Node != "worker-1" {
		t.Fatalf("unexpected pods: %+v", got.Pods)
	}
	if len(got.Services) != 1 || got.Services[0].Name != "checkout" {
		t.Fatalf("unrelated service leaked into inventory: %+v", got.Services)
	}
}

func ptr[T any](v T) *T { return &v }

func TestRunJobSucceeds(t *testing.T) {
	cs := fake.NewSimpleClientset()
	a := &Application{RepoURL: "https://git.example/app.git", ImageRepo: "reg/app", Dockerfile: "Dockerfile", ContextDir: "."}
	r := &Run{ID: 9, Branch: "main", ImageTag: "abc123"}
	spec := stagePod(a, r, Stage{Name: "build", Type: "build", RegistrySecret: "regcred"}, "cicd-run-9")
	if spec.Containers[0].Image != KanikoImage || !strings.Contains(strings.Join(spec.Containers[0].Args, " "), "--destination=reg/app:abc123") {
		t.Fatalf("bad kaniko spec: %+v", spec.Containers[0])
	}
	ctx := context.Background()
	go func() { // simulate the Job controller + kubelet
		for {
			j, err := cs.BatchV1().Jobs("ci").Get(ctx, "job1", metav1.GetOptions{})
			if err == nil {
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "job1-x", Namespace: "ci", Labels: map[string]string{"job-name": "job1"}},
					Status: corev1.PodStatus{Phase: corev1.PodSucceeded,
						InitContainerStatuses: []corev1.ContainerStatus{{Name: cloneContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}},
						ContainerStatuses:     []corev1.ContainerStatus{{Name: stepContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}}}
				_, _ = cs.CoreV1().Pods("ci").Create(ctx, pod, metav1.CreateOptions{})
				j.Status.Succeeded = 1
				j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
				_, _ = cs.BatchV1().Jobs("ci").UpdateStatus(ctx, j, metav1.UpdateOptions{})
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	var mu sync.Mutex
	var logs []string
	err := runJob(ctx, cs, "ci", "job1", 9, spec, 60, func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, f)
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) < 3 {
		t.Fatalf("expected streamed logs, got %v", logs)
	}
}

func TestSplitImageAndJobPath(t *testing.T) {
	if r, tg := splitImage("reg:5000/team/app:v2"); r != "reg:5000/team/app" || tg != "v2" {
		t.Fatal(r, tg)
	}
	if r, tg := splitImage("reg:5000/team/app"); r != "reg:5000/team/app" || tg != "latest" {
		t.Fatal(r, tg)
	}
	if jobPath("a/b c") != "/job/a/job/b%20c" {
		t.Fatal(jobPath("a/b c"))
	}
}
