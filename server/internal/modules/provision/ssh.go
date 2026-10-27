package provision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Output from remote commands is bounded and is never placed in task logs.
type limitedBuffer struct {
	b bytes.Buffer
	n int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.b.Len() < b.n {
		keep := b.n - b.b.Len()
		if keep > n {
			keep = n
		}
		_, _ = b.b.Write(p[:keep])
	}
	return n, nil
}

func connect(ctx context.Context, h Host, capture bool) (*ssh.Client, string, string, error) {
	fingerprint, publicKey := "", ""
	cfg := &ssh.ClientConfig{User: h.Username, Auth: []ssh.AuthMethod{ssh.Password(h.Password)}, Timeout: 15 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fingerprint = ssh.FingerprintSHA256(key)
			publicKey = key.Type() + " " + base64.StdEncoding.EncodeToString(key.Marshal())
			if !capture && fingerprint != h.Fingerprint {
				return errors.New("SSH host key changed")
			}
			return nil
		}}
	addr := net.JoinHostPort(h.IP, strconv.Itoa(h.Port))
	conn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, "", "", errors.New("SSH connection failed")
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	cc, ch, req, err := ssh.NewClientConn(conn, addr, cfg)
	stop()
	if err != nil {
		_ = conn.Close()
		return nil, "", "", errors.New("SSH authentication or host key verification failed")
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(cc, ch, req), fingerprint, publicKey, nil
}

func runSSH(ctx context.Context, h Host, script string, capture bool) (string, string, string, error) {
	client, fp, key, err := connect(ctx, h, capture)
	if err != nil {
		return "", fp, key, err
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	session, err := client.NewSession()
	if err != nil {
		return "", fp, key, errors.New("SSH session failed")
	}
	defer session.Close()
	session.Stdin = strings.NewReader(script)
	out := &limitedBuffer{n: 1024 * 1024}
	session.Stdout = out
	session.Stderr = io.Discard
	command := "sh -s"
	if h.Username != "root" {
		command = "sudo -n sh -s"
	}
	if err := session.Run(command); err != nil {
		return "", fp, key, errors.New("remote command failed (requires root or passwordless sudo)")
	}
	return out.b.String(), fp, key, nil
}

const preflightScript = `set -eu
test "$(id -u)" = 0
test "$(uname -s)" = Linux
case "$(uname -m)" in x86_64|aarch64) ;; *) exit 1;; esac
command -v systemctl >/dev/null
test -d /run/systemd/system
command -v curl >/dev/null
for f in /etc/rancher/k3s /var/lib/rancher/k3s /etc/kubernetes /var/lib/kubelet; do
  test ! -e "$f" || exit 1
done
for c in k3s kubelet kubeadm; do
  ! command -v "$c" >/dev/null 2>&1 || exit 1
done
test "$(getconf _NPROCESSORS_ONLN)" -ge 2
test "$(awk '/MemTotal/ { print $2 }' /proc/meminfo)" -ge "$MIN_RAM"
test "$(df -Pk /var | awk 'NR==2 {print $4}')" -ge 10485760
test -z "$(awk 'NR>1 {print}' /proc/swaps)"
printf 'OK %s\n' "$(uname -m)"
`

func preflight(ctx context.Context, h Host, master, capture bool, cni string) (string, string, error) {
	ram := "1048576"
	if master {
		ram = "2097152"
	}
	script := "MIN_RAM=" + ram + "\n" + preflightScript
	if cni == "wireguard-native" {
		script += "modprobe wireguard >/dev/null 2>&1\n"
	}
	out, fp, key, err := runSSH(ctx, h, script, capture)
	if err != nil || !strings.HasPrefix(out, "OK ") {
		return fp, key, fmt.Errorf("preflight failed for %s: verify fresh Linux/systemd, root or passwordless sudo, curl, CPU/RAM/disk and disabled swap", h.IP)
	}
	return fp, key, nil
}

func installScript(r *Request, p *Plan, index int, token string) string {
	h := r.Hosts[index]
	config := map[string]any{"token": token, "node-ip": h.IP, "node-name": r.Name + "-" + strconv.Itoa(index+1)}
	role := "agent"
	if index < p.Masters {
		role = "server"
		config["write-kubeconfig-mode"] = "0600"
		config["tls-san"] = []string{r.Endpoint, r.Hosts[0].IP}
		config["flannel-backend"] = r.CNI
		disabled := []string{}
		for _, c := range []struct {
			name string
			on   bool
		}{{"traefik", r.Components.Traefik}, {"servicelb", r.Components.ServiceLB}, {"local-storage", r.Components.LocalStorage}, {"metrics-server", r.Components.Metrics}} {
			if !c.on {
				disabled = append(disabled, c.name)
			}
		}
		config["disable"] = disabled
		if index == 0 && p.HA {
			config["cluster-init"] = true
		}
	}
	if index != 0 {
		config["server"] = "https://" + net.JoinHostPort(r.Hosts[0].IP, "6443")
	}
	data, _ := json.Marshal(config) // JSON is a YAML subset; no shell interpolation of user fields.
	script := "set -eu\numask 077\n" +
		"test ! -e /etc/rancher/k3s\ntest ! -e /etc/kubernetes\nmkdir -p /etc/rancher/k3s\n" +
		"printf %s " + shellQuote(base64.StdEncoding.EncodeToString(data)) + " | base64 -d > /etc/rancher/k3s/config.yaml\n" +
		"f=$(mktemp)\ntrap 'rm -f \"$f\"' EXIT\ncurl --connect-timeout 20 --max-time 120 -fsSL https://get.k3s.io -o \"$f\"\n" +
		"INSTALL_K3S_VERSION=" + shellQuote(r.Version) + " INSTALL_K3S_EXEC=" + shellQuote(role) + " sh \"$f\" >/dev/null 2>&1\n"
	if registryData, err := registryMirrorYAML(r.RegistryMirror); err == nil && len(registryData) > 0 {
		registrySetup := "printf %s " + shellQuote(base64.StdEncoding.EncodeToString(registryData)) +
			" | base64 -d > /etc/rancher/k3s/registries.yaml\n"
		script = strings.Replace(script, "f=$(mktemp)", registrySetup+"f=$(mktemp)", 1)
	}
	return script
}
