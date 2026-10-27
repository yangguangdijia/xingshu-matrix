package cluster

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

type networkProbeInput struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	UID       string `json:"uid"`
	Container string `json:"container"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
}

var probeDNSLabel = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

func validProbeHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	name := strings.TrimSuffix(host, ".")
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !probeDNSLabel.MatchString(label) {
			return false
		}
	}
	return true
}

func probeCommand(in networkProbeInput) ([]string, error) {
	if in.Namespace == "" || in.Pod == "" || in.UID == "" || in.Container == "" || in.Port < 1 || in.Port > 65535 {
		return nil, errors.New("namespace, Pod UID, container and port 1..65535 are required")
	}
	if !validProbeHost(in.Host) {
		return nil, errors.New("target must be an IP or DNS name without protocol, path or shell syntax")
	}
	// Arguments are sent directly to the container runtime, never a shell.
	return []string{"nc", "-z", "-w", "3", in.Host, strconv.Itoa(in.Port)}, nil
}

func (m *Module) networkProbe(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	var in networkProbeInput
	if !httpx.Bind(c, &in) {
		return
	}
	command, err := probeCommand(in)
	if err != nil {
		httpx.Fail(c, httpx.Validation(err.Error()))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 12*time.Second)
	defer cancel()
	pod, err := kc.cs.CoreV1().Pods(in.Namespace).Get(ctx, in.Pod, metav1.GetOptions{})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	if string(pod.UID) != in.UID || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		httpx.Fail(c, httpx.Conflict("source Pod changed or is not Running; reload the source inventory"))
		return
	}
	running := false
	for _, status := range pod.Status.ContainerStatuses {
		running = running || status.Name == in.Container && status.State.Running != nil
	}
	if !running {
		httpx.Fail(c, httpx.Conflict("selected container is not currently running"))
		return
	}
	u := kc.cs.CoreV1().RESTClient().Post().Resource("pods").Name(pod.Name).Namespace(pod.Namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: in.Container, Command: command, Stdout: true, Stderr: true}, scheme.ParameterCodec).URL()
	executor, err := remotecommand.NewSPDYExecutor(streamConfig(kc.rest), http.MethodPost, u)
	if err != nil {
		httpx.Fail(c, httpx.Conflict("cluster exec transport is unavailable"))
		return
	}
	started := time.Now()
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: io.Discard, Stderr: io.Discard})
	audit.SetAction(c, "network.probe")
	audit.SetResource(c, "pod", pod.Namespace+"/"+pod.Name)
	audit.SetDetail(c, gin.H{"host": in.Host, "port": in.Port, "reachable": err == nil})
	message := "源容器的 TCP 连接成功；仅代表本次探测，不代表所有来源或协议均可达"
	if err != nil {
		message = "探测未成功；请核对容器是否提供支持 -z/-w 的 nc、pods/exec 权限、DNS、目标监听端口、网络插件和策略。不能单凭本次失败认定 NetworkPolicy 阻断。"
	}
	httpx.OK(c, gin.H{"reachable": err == nil, "message": message, "duration_ms": time.Since(started).Milliseconds(),
		"source": pod.Namespace + "/" + pod.Name + "/" + in.Container, "host": in.Host, "port": in.Port})
}
