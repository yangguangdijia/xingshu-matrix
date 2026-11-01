package cluster

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

var integrationTypes = []string{"prometheus", "loki", "jaeger", "tempo", "alertmanager", "elasticsearch", "fluent-bit"}

type integrationEvidence struct {
	Source    string `json:"source"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Match     string `json:"match"`
}

type integrationPort struct {
	Name     string `json:"name"`
	Port     int32  `json:"port"`
	Protocol string `json:"protocol"`
	NodePort int32  `json:"node_port,omitempty"`
}

// Scope describes where a discovered address might be routable; none of
// these scopes is evidence that Matrix Server can reach the endpoint.
type integrationAddress struct {
	URL    string `json:"url"`
	Host   string `json:"host"`
	Port   int32  `json:"port"`
	Scope  string `json:"scope"`
	Source string `json:"source"`
}

type integrationCandidate struct {
	Key                string                `json:"key"`
	Type               string                `json:"type"`
	Name               string                `json:"name"`
	Namespace          string                `json:"namespace"`
	Kind               string                `json:"kind"`
	Service            string                `json:"service"`
	Host               string                `json:"host,omitempty"`
	Ports              []integrationPort     `json:"ports"`
	URLCandidates      []string              `json:"url_candidates"`
	Addresses          []integrationAddress  `json:"addresses"`
	State              string                `json:"state"`
	Truncated          bool                  `json:"truncated"`
	DiscoveredAt       time.Time             `json:"discovered_at"`
	Reachability       string                `json:"reachability"`
	Collector          bool                  `json:"collector"`
	Evidence           []integrationEvidence `json:"evidence"`
	ClusterID          int64                 `json:"cluster_id"`
	ConnectionVerified bool                  `json:"connection_verified"`
}

type integrationCandidateResult struct {
	ClusterID    int64                        `json:"cluster_id"`
	Items        []integrationCandidate       `json:"items"`
	Components   map[string]networkReadStatus `json:"components"`
	Sources      map[string]networkReadStatus `json:"sources"`
	Truncated    bool                         `json:"truncated"`
	DiscoveredAt time.Time                    `json:"discovered_at"`
}

type integrationWorkload struct {
	kind, name, namespace string
	labels                map[string]string
	images                []string
	hostIP                string
}

func parseIntegrationTypes(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return append([]string{}, integrationTypes...), nil
	}
	out := []string{}
	seen := map[string]bool{}
	for _, value := range strings.Split(raw, ",") {
		value = strings.TrimSpace(value)
		valid := false
		for _, supported := range integrationTypes {
			valid = valid || supported == value
		}
		if !valid {
			return nil, httpx.BadRequest("unsupported integration type")
		}
		if !seen[value] {
			out, seen[value] = append(out, value), true
		}
	}
	return out, nil
}

func (m *Module) integrationCandidates(c *gin.Context) {
	wanted, err := parseIntegrationTypes(c.Query("types"))
	if err == nil {
		err = validNetworkScope(c.Query("namespace"), "")
	}
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	cl, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	httpx.OK(c, readIntegrationCandidates(c.Request.Context(), kc, cl.ID, c.Query("namespace"), wanted))
}

func readIntegrationCandidates(ctx context.Context, kc *kubeClients, clusterID int64, namespace string, wanted []string) integrationCandidateResult {
	out := integrationCandidateResult{
		ClusterID: clusterID, Items: []integrationCandidate{},
		Components: map[string]networkReadStatus{}, Sources: map[string]networkReadStatus{},
		DiscoveredAt: time.Now().UTC(),
	}
	if kc == nil || kc.cs == nil || kc.dyn == nil {
		for _, component := range wanted {
			out.Components[component] = networkReadStatus{State: "unavailable", Error: "cluster client unavailable"}
		}
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	opts := metav1.ListOptions{Limit: networkReadLimit}
	listCtx, listCancel := context.WithTimeout(ctx, networkReadTimeout)
	services, err := kc.cs.CoreV1().Services(namespace).List(listCtx, opts)
	listCancel()
	serviceItems := []corev1.Service{}
	if err != nil {
		out.Sources["services"] = networkReadFailure(err)
	} else {
		count := len(services.Items)
		if count > networkReadLimit {
			count = networkReadLimit
		}
		for _, service := range services.Items[:count] {
			if namespace == "" || service.Namespace == namespace {
				serviceItems = append(serviceItems, service)
			}
		}
		out.Sources["services"] = networkReadStatus{Available: true, State: "available",
			Truncated: services.Continue != "" || len(services.Items) > count}
	}
	workloads := []integrationWorkload{}
	for _, resource := range []struct {
		gvr  schema.GroupVersionResource
		kind string
	}{
		{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, "Deployment"},
		{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, "StatefulSet"},
		{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}, "DaemonSet"},
		{schema.GroupVersionResource{Version: "v1", Resource: "pods"}, "Pod"},
	} {
		listCtx, listCancel := context.WithTimeout(ctx, networkReadTimeout)
		list, err := kc.dyn.Resource(resource.gvr).Namespace(namespace).List(listCtx, opts)
		listCancel()
		if err != nil {
			out.Sources[resource.gvr.Resource] = networkReadFailure(err)
			continue
		}
		count := len(list.Items)
		if count > networkReadLimit {
			count = networkReadLimit
		}
		out.Sources[resource.gvr.Resource] = networkReadStatus{Available: true, State: "available",
			Truncated: list.GetContinue() != "" || len(list.Items) > count}
		for _, obj := range list.Items[:count] {
			if namespace != "" && obj.GetNamespace() != namespace {
				continue
			}
			workloads = append(workloads, integrationWorkloadRow(obj, resource.kind))
		}
	}
	for _, source := range out.Sources {
		out.Truncated = out.Truncated || source.Truncated
	}
	for _, component := range wanted {
		candidates := identifyIntegrationCandidates(component, clusterID, serviceItems, workloads)
		remaining := networkReadLimit - len(out.Items)
		truncated := len(candidates) > remaining
		if truncated {
			candidates = candidates[:remaining]
			out.Truncated = true
		}
		out.Items = append(out.Items, candidates...)
		out.Components[component] = integrationComponentStatus(candidates, out.Sources, truncated)
	}
	out.DiscoveredAt = time.Now().UTC()
	for i := range out.Items {
		out.Items[i].Truncated = out.Truncated
		out.Items[i].DiscoveredAt = out.DiscoveredAt
	}
	sort.Slice(out.Items, func(i, j int) bool {
		a, b := out.Items[i], out.Items[j]
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
	return out
}

func integrationWorkloadRow(obj unstructured.Unstructured, kind string) integrationWorkload {
	row := integrationWorkload{kind: kind, name: obj.GetName(), namespace: obj.GetNamespace(), labels: obj.GetLabels()}
	containerPath := []string{"spec", "containers"}
	if kind != "Pod" {
		containerPath = []string{"spec", "template", "spec", "containers"}
		row.labels, _, _ = unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "labels")
	}
	containers, _, _ := unstructured.NestedSlice(obj.Object, containerPath...)
	for _, container := range containers {
		if entry, ok := container.(map[string]any); ok {
			if image, ok := entry["image"].(string); ok {
				row.images = append(row.images, image)
			}
		}
	}
	if kind == "Pod" {
		row.hostIP, _, _ = unstructured.NestedString(obj.Object, "status", "hostIP")
	}
	return row
}

func integrationMarker(value, component string) bool {
	value = strings.ToLower(value)
	if component == "prometheus" {
		for _, excluded := range []string{"exporter", "operator", "adapter", "pushgateway", "alertmanager"} {
			if strings.Contains(value, excluded) {
				return false
			}
		}
	}
	if component == "elasticsearch" && strings.Contains(value, "exporter") {
		return false
	}
	markers := []string{component}
	if component == "fluent-bit" {
		markers = append(markers, "fluentbit")
	}
	for _, marker := range markers {
		// Match a complete name/image segment, not e.g. "notprometheus".
		tokens := strings.FieldsFunc(value, func(r rune) bool {
			return r == '.' || r == '_' || r == '/' || r == ':' || r == '@'
		})
		for _, token := range tokens {
			if token == marker || strings.HasPrefix(token, marker+"-") ||
				strings.HasSuffix(token, "-"+marker) || strings.Contains(token, "-"+marker+"-") {
				return true
			}
		}
	}
	return false
}

func integrationMatch(name string, objectLabels map[string]string, images []string, component string) string {
	if integrationMarker(name, component) {
		return "name"
	}
	for _, key := range []string{"app.kubernetes.io/name", "app", "k8s-app", "app.kubernetes.io/component"} {
		if integrationMarker(objectLabels[key], component) {
			return "label:" + key
		}
	}
	for _, image := range images {
		// Registry/organization names are not component evidence.
		if i := strings.LastIndex(image, "/"); i >= 0 {
			image = image[i+1:]
		}
		if integrationMarker(image, component) {
			return "container_image"
		}
	}
	return ""
}

func identifyIntegrationCandidates(component string, clusterID int64, services []corev1.Service, workloads []integrationWorkload) []integrationCandidate {
	out := []integrationCandidate{}
	matches := []integrationWorkload{}
	matchEvidence := []integrationEvidence{}
	for _, workload := range workloads {
		match := integrationMatch(workload.name, workload.labels, workload.images, component)
		if match != "" {
			matches = append(matches, workload)
			matchEvidence = append(matchEvidence, integrationEvidence{Source: "workload", Kind: workload.kind,
				Name: workload.name, Namespace: workload.namespace, Match: match})
		}
	}
	linked := map[int]bool{}
	for _, service := range services {
		evidence := []integrationEvidence{}
		hostIPs := []string{}
		direct := integrationMatch(service.Name, service.Labels, nil, component)
		if direct != "" {
			evidence = append(evidence, integrationEvidence{Source: "service", Kind: "Service",
				Name: service.Name, Namespace: service.Namespace, Match: direct})
		}
		for index, workload := range matches {
			if workload.namespace != service.Namespace || len(service.Spec.Selector) == 0 ||
				!labels.SelectorFromSet(service.Spec.Selector).Matches(labels.Set(workload.labels)) {
				continue
			}
			linked[index] = true
			if len(evidence) < 32 {
				item := matchEvidence[index]
				item.Match = "service_selector:" + item.Match
				evidence = append(evidence, item)
			}
		}
		if len(evidence) == 0 {
			continue
		}
		// Real Pod host addresses are NodePort candidates, never a guessed
		// API server host. Service and Pod must be in the same namespace.
		for _, workload := range workloads {
			if workload.kind == "Pod" && workload.namespace == service.Namespace && len(service.Spec.Selector) > 0 &&
				labels.SelectorFromSet(service.Spec.Selector).Matches(labels.Set(workload.labels)) &&
				net.ParseIP(workload.hostIP) != nil && len(hostIPs) < 16 {
				hostIPs = append(hostIPs, workload.hostIP)
			}
		}
		candidate := integrationCandidate{
			Key:  integrationCandidateKey(clusterID, component, service.Namespace, "Service", service.Name),
			Type: component, Name: service.Name, Namespace: service.Namespace, Kind: "Service",
			Service: service.Name, Host: service.Name + "." + service.Namespace + ".svc",
			Ports: []integrationPort{}, URLCandidates: []string{}, Addresses: []integrationAddress{},
			State: "unavailable", Reachability: "not_tested", Collector: component == "fluent-bit",
			Evidence: evidence, ClusterID: clusterID,
		}
		for i, port := range service.Spec.Ports {
			if i >= 64 {
				break
			}
			candidate.Ports = append(candidate.Ports, integrationPort{Name: port.Name, Port: port.Port,
				Protocol: string(port.Protocol), NodePort: port.NodePort})
		}
		if component != "fluent-bit" {
			candidate.Addresses = integrationServiceAddresses(service, component, hostIPs)
			for _, address := range candidate.Addresses {
				candidate.URLCandidates = append(candidate.URLCandidates, address.URL)
			}
		}
		if len(candidate.URLCandidates) > 0 || component == "fluent-bit" {
			candidate.State = "available"
		}
		out = append(out, candidate)
	}
	for index, workload := range matches {
		if linked[index] {
			continue
		}
		state := "unavailable"
		if component == "fluent-bit" {
			state = "available"
		}
		out = append(out, integrationCandidate{
			Key:  integrationCandidateKey(clusterID, component, workload.namespace, workload.kind, workload.name),
			Type: component, Name: workload.name, Namespace: workload.namespace, Kind: workload.kind,
			Ports: []integrationPort{}, URLCandidates: []string{}, Addresses: []integrationAddress{}, State: state,
			Reachability: "not_tested", Collector: component == "fluent-bit",
			Evidence: []integrationEvidence{matchEvidence[index]}, ClusterID: clusterID,
		})
	}
	return out
}

func integrationCandidateKey(clusterID int64, component, namespace, kind, name string) string {
	return fmt.Sprintf("%d:%s:%s:%s:%s", clusterID, component, namespace, kind, name)
}

func integrationServiceURLs(service corev1.Service, component string, hostIPs []string) []string {
	out := []string{}
	for _, address := range integrationServiceAddresses(service, component, hostIPs) {
		out = append(out, address.URL)
	}
	return out
}

func integrationServiceAddresses(service corev1.Service, component string, hostIPs []string) []integrationAddress {
	ports := map[string]int32{"prometheus": 9090, "loki": 3100, "jaeger": 16686, "tempo": 3200,
		"alertmanager": 9093, "elasticsearch": 9200}
	out := []integrationAddress{}
	seen := map[string]bool{}
	add := func(host string, port int32, scheme, scope, source string) {
		// URL construction accepts only plain DNS names and IP addresses.
		if host == "" || strings.ContainsAny(host, "/@?#% \t\r\n") || port <= 0 || port > 65535 {
			return
		}
		if net.ParseIP(host) == nil && len(validation.IsDNS1123Subdomain(host)) != 0 {
			return
		}
		value := (&url.URL{Scheme: scheme, Host: net.JoinHostPort(host, strconv.Itoa(int(port)))}).String()
		if !seen[value] && len(out) < 64 {
			seen[value], out = true, append(out, integrationAddress{
				URL: value, Host: host, Port: port, Scope: scope, Source: source,
			})
		}
	}
	for _, port := range service.Spec.Ports {
		name := strings.ToLower(port.Name)
		if port.Protocol != "" && port.Protocol != corev1.ProtocolTCP {
			continue
		}
		queryPort := port.Port == ports[component] || port.Port == 80 || port.Port == 443 ||
			name == "http" || name == "web" || name == "query" || name == "https" ||
			name == "http-query" || name == "http-web"
		if !queryPort || strings.Contains(name, "grpc") || strings.Contains(name, "otlp") ||
			strings.Contains(name, "metrics") || strings.Contains(name, "ingest") {
			continue
		}
		scheme := "http"
		if port.Port == 443 || name == "https" {
			scheme = "https"
		}
		add(service.Name+"."+service.Namespace+".svc", port.Port, scheme, "cluster_internal", "service_dns")
		for i, address := range service.Status.LoadBalancer.Ingress {
			if i >= 16 {
				break
			}
			add(address.IP, port.Port, scheme, "external_candidate", "load_balancer")
			add(address.Hostname, port.Port, scheme, "external_candidate", "load_balancer")
		}
		if port.NodePort > 0 {
			for _, hostIP := range hostIPs {
				add(hostIP, port.NodePort, scheme, "node_network", "node_port")
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}

func integrationComponentStatus(candidates []integrationCandidate, sources map[string]networkReadStatus, truncated bool) networkReadStatus {
	out := networkReadStatus{State: "missing", Error: "no matching component evidence in the readable inventory", Truncated: truncated}
	for _, source := range sources {
		out.Truncated = out.Truncated || source.Truncated
		if source.State == "forbidden" {
			out.State, out.Error = "forbidden", "component discovery is incomplete: a resource cannot be read"
		} else if source.State == "error" && out.State != "forbidden" {
			out.State, out.Error = "error", "component discovery failed"
		} else if !source.Available && out.State == "missing" {
			out.State, out.Error = "unavailable", "component discovery is incomplete: a resource API is unavailable"
		}
	}
	if out.Truncated && out.State == "missing" {
		out.State, out.Error = "unavailable", "component discovery inventory was truncated"
	}
	for _, candidate := range candidates {
		if candidate.State == "available" {
			out.Available, out.State, out.Error = true, "available", ""
			return out
		}
	}
	if len(candidates) > 0 && out.State == "missing" {
		out.State, out.Error = "unavailable", "component evidence found without a query endpoint"
	}
	return out
}
