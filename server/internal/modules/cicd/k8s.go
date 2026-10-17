package cicd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type kubeIface = kubernetes.Interface

// Container images used by runner Jobs.
var (
	GitImage    = "alpine/git:2.45.2"
	KanikoImage = "gcr.io/kaniko-project/executor:v1.23.2"
	HelmImage   = "alpine/helm:3.16.3"
)

const (
	workDir         = "/workspace"
	srcDir          = "/workspace/src"
	stepContainer   = "step"
	cloneContainer  = "git-clone"
	defaultJobLimit = 3600
)

func urlUserinfo(user, pass string) string { return url.UserPassword(user, pass).String() }

const cloneScript = `set -eo pipefail
git clone --depth 1 --branch "$BRANCH" "$GIT_URL" ` + srcDir + ` 2>&1 | sed -E 's#://[^@/]+@#://***@#g'
cd ` + srcDir + `
if [ -n "$COMMIT" ]; then git fetch --depth 1 origin "$COMMIT" && git checkout -q FETCH_HEAD; fi
git log -1 --format='checked out %H %s'`

// stagePod builds the pod spec for a build/test/script stage: a git-clone
// initContainer fills an emptyDir that the step container works in.
func stagePod(a *Application, r *Run, s Stage, secret string) corev1.PodSpec {
	vols := []corev1.Volume{{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	mounts := []corev1.VolumeMount{{Name: "workspace", MountPath: workDir}}
	env := []corev1.EnvVar{
		{Name: "CI", Value: "true"}, {Name: "CI_RUN_ID", Value: fmt.Sprint(r.ID)},
		{Name: "CI_BRANCH", Value: r.Branch}, {Name: "CI_COMMIT", Value: r.Commit}, {Name: "CI_IMAGE_TAG", Value: r.ImageTag},
	}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, corev1.EnvVar{Name: k, Value: s.Env[k]})
	}
	step := corev1.Container{Name: stepContainer, Env: env, VolumeMounts: mounts, WorkingDir: srcDir}
	switch s.Type {
	case "build":
		img := s.Image
		if img == "" {
			img = a.ImageRepo + ":" + r.ImageTag
		}
		step.Image = KanikoImage
		step.Args = append([]string{
			"--context=dir://" + path.Join(srcDir, a.ContextDir),
			"--dockerfile=" + path.Join(srcDir, a.Dockerfile),
			"--destination=" + img,
		}, s.ExtraArgs...)
		if s.RegistrySecret != "" {
			vols = append(vols, corev1.Volume{Name: "docker-config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: s.RegistrySecret, Items: []corev1.KeyToPath{{Key: ".dockerconfigjson", Path: "config.json"}}}}})
			step.VolumeMounts = append(step.VolumeMounts, corev1.VolumeMount{Name: "docker-config", MountPath: "/kaniko/.docker", ReadOnly: true})
		}
	default: // test / script
		step.Image = s.Image
		step.Command = []string{"sh", "-c", "set -e\n" + strings.Join(s.Commands, "\n")}
	}
	clone := corev1.Container{
		Name: cloneContainer, Image: GitImage, Command: []string{"sh", "-c", cloneScript}, VolumeMounts: mounts,
		Env: []corev1.EnvVar{
			{Name: "GIT_URL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: "git_url"}}},
			{Name: "BRANCH", Value: r.Branch}, {Name: "COMMIT", Value: r.Commit},
		},
	}
	return corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, InitContainers: []corev1.Container{clone},
		Containers: []corev1.Container{step}, Volumes: vols}
}

var fatalWaiting = map[string]bool{"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true,
	"CreateContainerConfigError": true, "CreateContainerError": true}

// runJob creates a Job from spec, streams its containers' logs to logf, and
// waits for completion. Cancelling ctx deletes the Job.
func runJob(ctx context.Context, cs kubeIface, ns, name string, runID int64, spec corev1.PodSpec, timeoutSec int, logf func(string, ...any)) error {
	if timeoutSec <= 0 {
		timeoutSec = defaultJobLimit
	}
	deadline, backoff, ttl := int64(timeoutSec), int32(0), int32(3600)
	labels := map[string]string{"app.kubernetes.io/managed-by": "matrix-cicd", "matrix.cicd/run": fmt.Sprint(runID)}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: batchv1.JobSpec{BackoffLimit: &backoff, ActiveDeadlineSeconds: &deadline, TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: spec}},
	}
	jobs := cs.BatchV1().Jobs(ns)
	bg := metav1.DeletePropagationBackground
	_ = jobs.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &bg}) // stale job from a retried run
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create job: %w", err)
	}
	logf("job %s/%s created", ns, name)

	containers := []string{}
	for _, c := range spec.InitContainers {
		containers = append(containers, c.Name)
	}
	for _, c := range spec.Containers {
		containers = append(containers, c.Name)
	}
	logsDone := make(chan struct{})
	lctx, lcancel := context.WithCancel(context.Background())
	defer lcancel()
	go func() { defer close(logsDone); streamJobLogs(lctx, cs, ns, name, containers, logf) }()
	finish := func(err error) error {
		select { // give the log stream a moment to drain the tail
		case <-logsDone:
		case <-time.After(10 * time.Second):
		}
		return err
	}

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = jobs.Delete(context.Background(), name, metav1.DeleteOptions{PropagationPolicy: &bg})
			logf("job %s deleted (cancelled)", name)
			lcancel()
			return finish(ctx.Err())
		case <-tick.C:
		}
		j, err := jobs.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				lcancel()
				return finish(errors.New("job disappeared"))
			}
			continue
		}
		if j.Status.Succeeded > 0 {
			return finish(nil)
		}
		if j.Status.Failed > 0 || jobCondition(j, batchv1.JobFailed) {
			msg := "job failed"
			for _, c := range j.Status.Conditions {
				if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
					msg = fmt.Sprintf("job failed: %s %s", c.Reason, c.Message)
				}
			}
			return finish(errors.New(msg))
		}
		if reason := podFatal(ctx, cs, ns, name); reason != "" {
			_ = jobs.Delete(context.Background(), name, metav1.DeleteOptions{PropagationPolicy: &bg})
			lcancel()
			return finish(errors.New(reason))
		}
	}
}

func jobCondition(j *batchv1.Job, t batchv1.JobConditionType) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobPod(ctx context.Context, cs kubeIface, ns, job string) *corev1.Pod {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + job})
	if err != nil || len(pods.Items) == 0 {
		return nil
	}
	return &pods.Items[len(pods.Items)-1]
}

// podFatal reports a container stuck in an unrecoverable waiting state (the Job would otherwise hang until its deadline).
func podFatal(ctx context.Context, cs kubeIface, ns, job string) string {
	p := jobPod(ctx, cs, ns, job)
	if p == nil {
		return ""
	}
	for _, st := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if w := st.State.Waiting; w != nil && fatalWaiting[w.Reason] {
			return fmt.Sprintf("container %s: %s: %s", st.Name, w.Reason, w.Message)
		}
	}
	return ""
}

func containerStarted(p *corev1.Pod, name string) bool {
	for _, st := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if st.Name == name {
			return st.State.Running != nil || st.State.Terminated != nil
		}
	}
	return false
}

// streamJobLogs follows each container's logs in order once it has started.
func streamJobLogs(ctx context.Context, cs kubeIface, ns, job string, containers []string, logf func(string, ...any)) {
	for _, cn := range containers {
		var pod *corev1.Pod
		for pod == nil || !containerStarted(pod, cn) {
			if pod != nil && (pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded) && !containerStarted(pod, cn) {
				return // never ran (earlier init container failed)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			pod = jobPod(ctx, cs, ns, job)
		}
		logf("--- [%s] ---", cn)
		rc, err := cs.CoreV1().Pods(ns).GetLogs(pod.Name, &corev1.PodLogOptions{Container: cn, Follow: true}).Stream(ctx)
		if err != nil {
			logf("[log stream error] %v", err)
			continue
		}
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			logf("%s", sc.Text())
		}
		rc.Close()
	}
}
