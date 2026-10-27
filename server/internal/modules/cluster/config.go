package cluster

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// Configuration objects are namespaced data. Secret values never leave the
// server: list/get responses contain metadata and key names only, while
// create/update accept plaintext over the authenticated API and write it
// straight into the Kubernetes Secret. Error messages are built from API
// status text, never from the submitted values.

const (
	PermConfigRead  = "config:read"
	PermConfigWrite = "config:write"

	maxConfigBytes = 1 << 20 // Kubernetes object limit is 1 MiB
)

// ConfigMapRow is a ConfigMap with its full non-secret data.
type ConfigMapRow struct {
	Namespace       string            `json:"namespace"`
	Name            string            `json:"name"`
	UID             types.UID         `json:"uid"`
	ResourceVersion string            `json:"resource_version"`
	Keys            []string          `json:"keys"`
	Data            map[string]string `json:"data"`
	BinaryData      map[string]string `json:"binary_data"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
	Immutable       bool              `json:"immutable"`
	CreatedAt       metav1.Time       `json:"created_at"`
}

// SecretRow is Secret metadata only. It intentionally has no data field.
type SecretRow struct {
	Namespace       string            `json:"namespace"`
	Name            string            `json:"name"`
	UID             types.UID         `json:"uid"`
	ResourceVersion string            `json:"resource_version"`
	Type            string            `json:"type"`
	Keys            []string          `json:"keys"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
	Immutable       bool              `json:"immutable"`
	CreatedAt       metav1.Time       `json:"created_at"`
}

func configMapRow(cm corev1.ConfigMap) ConfigMapRow {
	keys := make([]string, 0, len(cm.Data)+len(cm.BinaryData))
	for k := range cm.Data {
		keys = append(keys, k)
	}
	for k := range cm.BinaryData {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	data := map[string]string{}
	binaryData := map[string]string{}
	for k, v := range cm.Data {
		data[k] = v
	}
	for k, v := range cm.BinaryData {
		binaryData[k] = base64.StdEncoding.EncodeToString(v)
	}
	return ConfigMapRow{Namespace: cm.Namespace, Name: cm.Name, UID: cm.UID, ResourceVersion: cm.ResourceVersion,
		Keys: keys, Data: data, BinaryData: binaryData,
		Labels: cm.Labels, Annotations: cm.Annotations, Immutable: cm.Immutable != nil && *cm.Immutable,
		CreatedAt: cm.CreationTimestamp}
}

func secretRow(sec corev1.Secret) SecretRow {
	keys := make([]string, 0, len(sec.Data)+len(sec.StringData))
	for k := range sec.Data {
		keys = append(keys, k)
	}
	for k := range sec.StringData {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return SecretRow{Namespace: sec.Namespace, Name: sec.Name, UID: sec.UID, ResourceVersion: sec.ResourceVersion,
		Type: string(sec.Type), Keys: keys,
		Labels: sec.Labels, Annotations: sec.Annotations, Immutable: sec.Immutable != nil && *sec.Immutable,
		CreatedAt: sec.CreationTimestamp}
}

// ---------------------------------------------------------------- read handlers

func (m *Module) listConfigMaps(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	list, err := kc.cs.CoreV1().ConfigMaps(c.Query("namespace")).List(c.Request.Context(), metav1.ListOptions{})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	keyword := strings.ToLower(strings.TrimSpace(c.Query("keyword")))
	items := make([]ConfigMapRow, 0, len(list.Items))
	for _, cm := range list.Items {
		row := configMapRow(cm)
		if keyword != "" && !strings.Contains(strings.ToLower(row.Namespace+"/"+row.Name+" "+strings.Join(row.Keys, " ")), keyword) {
			continue
		}
		items = append(items, row)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace == items[j].Namespace {
			return items[i].Name < items[j].Name
		}
		return items[i].Namespace < items[j].Namespace
	})
	httpx.OK(c, gin.H{"items": items, "total": len(items)})
}

func (m *Module) getConfigMap(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	cm, err := kc.cs.CoreV1().ConfigMaps(c.Param("ns")).Get(c.Request.Context(), c.Param("name"), metav1.GetOptions{})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	httpx.OK(c, configMapRow(*cm))
}

func (m *Module) listSecrets(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	list, err := kc.cs.CoreV1().Secrets(c.Query("namespace")).List(c.Request.Context(), metav1.ListOptions{})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	keyword := strings.ToLower(strings.TrimSpace(c.Query("keyword")))
	items := make([]SecretRow, 0, len(list.Items))
	for _, sec := range list.Items {
		row := secretRow(sec)
		if keyword != "" && !strings.Contains(strings.ToLower(row.Namespace+"/"+row.Name+" "+strings.Join(row.Keys, " ")+" "+row.Type), keyword) {
			continue
		}
		items = append(items, row)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace == items[j].Namespace {
			return items[i].Name < items[j].Name
		}
		return items[i].Namespace < items[j].Namespace
	})
	httpx.OK(c, gin.H{"items": items, "total": len(items)})
}

func (m *Module) getSecret(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	sec, err := kc.cs.CoreV1().Secrets(c.Param("ns")).Get(c.Request.Context(), c.Param("name"), metav1.GetOptions{})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	// Metadata only: never return sec.Data or sec.StringData.
	httpx.OK(c, secretRow(*sec))
}

// ---------------------------------------------------------------- payloads

type configMapParams struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	Data            map[string]string `json:"data"`
	BinaryData      map[string]string `json:"binary_data"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
	Immutable       *bool             `json:"immutable"`
	UID             types.UID         `json:"uid"`
	ResourceVersion string            `json:"resource_version"`
}

type secretParams struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	Type            string            `json:"type"`
	Data            map[string]string `json:"data"` // plaintext input, never returned
	Merge           bool              `json:"merge"`
	DeleteKeys      []string          `json:"delete_keys"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
	Immutable       *bool             `json:"immutable"`
	UID             types.UID         `json:"uid"`
	ResourceVersion string            `json:"resource_version"`
}

type configBatchItem struct {
	Namespace       string    `json:"namespace"`
	Name            string    `json:"name"`
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resource_version"`
}

type configBatchParams struct {
	Items   []configBatchItem `json:"items"`
	Confirm string            `json:"confirm"`
}

func configSize(data map[string]string) int {
	n := 0
	for k, v := range data {
		n += len(k) + len(v)
	}
	return n
}

func sameStringMap(current, next map[string]string) bool {
	if len(current) != len(next) {
		return false
	}
	for key, value := range current {
		nextValue, exists := next[key]
		if !exists || nextValue != value {
			return false
		}
	}
	return true
}

func sameBytesData(current, next map[string][]byte) bool {
	if len(current) != len(next) {
		return false
	}
	for key, value := range current {
		nextValue, exists := next[key]
		if !exists || !bytes.Equal(value, nextValue) {
			return false
		}
	}
	return true
}

func decodeConfigBinaryData(data map[string]string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(data))
	for key, value := range data {
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, httpx.BadRequest("binary_data values must be valid base64")
		}
		out[key] = decoded
	}
	return out, nil
}

func validateConfigIdentity(namespace, name string) error {
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		return httpx.BadRequest("invalid namespace")
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return httpx.BadRequest("invalid object name")
	}
	return nil
}

func validateConfigMapKeys(data map[string]string) error {
	for key := range data {
		if errs := validation.IsConfigMapKey(key); len(errs) > 0 {
			return httpx.BadRequest("invalid ConfigMap key " + key)
		}
	}
	return nil
}

func validateSecretKeys(data map[string]string) error {
	for key := range data {
		if errs := validation.IsConfigMapKey(key); len(errs) > 0 {
			return httpx.BadRequest("invalid Secret key " + key)
		}
	}
	return nil
}

func validateConfigChange(pl *changePayload) error {
	switch pl.Action {
	case "configmap_create", "configmap_update":
		var p configMapParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if pl.Action == "configmap_update" {
			p.Name, p.Namespace = pl.Name, pl.Namespace
		}
		if p.Namespace == "" {
			p.Namespace = pl.Namespace
		}
		if p.Name == "" {
			p.Name = pl.Name
		}
		if err := validateConfigIdentity(p.Namespace, p.Name); err != nil {
			return err
		}
		if err := validateConfigMapKeys(p.Data); err != nil {
			return err
		}
		if err := validateConfigMapKeys(p.BinaryData); err != nil {
			return err
		}
		binaryData, err := decodeConfigBinaryData(p.BinaryData)
		if err != nil {
			return err
		}
		size := configSize(p.Data)
		for key, value := range binaryData {
			size += len(key) + len(value)
			if _, exists := p.Data[key]; exists {
				return httpx.BadRequest("a key cannot exist in both data and binary_data")
			}
		}
		if size > maxConfigBytes {
			return httpx.BadRequest("ConfigMap data exceeds 1 MiB")
		}
		if pl.Action == "configmap_update" && (p.UID == "" || p.ResourceVersion == "") {
			return httpx.BadRequest("uid and resource_version are required for ConfigMap update")
		}
		if pl.Action == "configmap_create" && (p.UID != "" || p.ResourceVersion != "") {
			return httpx.BadRequest("create must not include uid or resource_version")
		}
		pl.Namespace, pl.Name = p.Namespace, p.Name
		normalized, _ := json.Marshal(p)
		pl.Params = normalized
	case "configmap_delete":
		if err := validateConfigIdentity(pl.Namespace, pl.Name); err != nil {
			return err
		}
		if err := validateResourcePreconditions(pl.Params, "ConfigMap"); err != nil {
			return err
		}
	case "secret_create", "secret_update":
		var p secretParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if pl.Action == "secret_update" {
			p.Name, p.Namespace = pl.Name, pl.Namespace
		}
		if p.Namespace == "" {
			p.Namespace = pl.Namespace
		}
		if p.Name == "" {
			p.Name = pl.Name
		}
		if err := validateConfigIdentity(p.Namespace, p.Name); err != nil {
			return err
		}
		if err := validateSecretKeys(p.Data); err != nil {
			return err
		}
		if configSize(p.Data) > maxConfigBytes {
			return httpx.BadRequest("Secret data exceeds 1 MiB")
		}
		if p.Type == "" {
			p.Type = string(corev1.SecretTypeOpaque)
		}
		if pl.Action == "secret_create" && (p.Merge || len(p.DeleteKeys) > 0) {
			return httpx.BadRequest("merge and delete_keys are only valid for Secret updates")
		}
		deleteKeys := map[string]bool{}
		for _, key := range p.DeleteKeys {
			if errs := validation.IsConfigMapKey(key); len(errs) > 0 {
				return httpx.BadRequest("invalid Secret key " + key)
			}
			if deleteKeys[key] {
				return httpx.BadRequest("duplicate Secret delete key")
			}
			deleteKeys[key] = true
			if p.Merge {
				if _, exists := p.Data[key]; exists {
					return httpx.BadRequest("a Secret key cannot be replaced and deleted in the same update")
				}
			}
		}
		if pl.Action == "secret_update" && len(p.DeleteKeys) > 0 && !p.Merge {
			return httpx.BadRequest("delete_keys requires merge=true")
		}
		if pl.Action == "secret_update" && configSize(p.Data) > maxConfigBytes {
			return httpx.BadRequest("Secret data exceeds 1 MiB")
		}
		if pl.Action == "secret_update" && (p.UID == "" || p.ResourceVersion == "") {
			return httpx.BadRequest("uid and resource_version are required for Secret update")
		}
		if pl.Action == "secret_create" && (p.UID != "" || p.ResourceVersion != "") {
			return httpx.BadRequest("create must not include uid or resource_version")
		}
		pl.Namespace, pl.Name = p.Namespace, p.Name
		normalized, _ := json.Marshal(p)
		pl.Params = normalized
	case "secret_delete":
		if err := validateConfigIdentity(pl.Namespace, pl.Name); err != nil {
			return err
		}
		if err := validateResourcePreconditions(pl.Params, "Secret"); err != nil {
			return err
		}
	case "configmap_batch_delete", "secret_batch_delete":
		var p configBatchParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if len(p.Items) == 0 || len(p.Items) > 100 {
			return httpx.BadRequest("items must contain 1..100 objects")
		}
		if p.Confirm != "DELETE" {
			return httpx.BadRequest("confirm=DELETE is required for batch deletion")
		}
		for _, item := range p.Items {
			if err := validateConfigIdentity(item.Namespace, item.Name); err != nil {
				return err
			}
			if item.UID == "" || item.ResourceVersion == "" {
				return httpx.BadRequest("each item needs uid and resource_version")
			}
		}
	default:
		return httpx.BadRequest("unknown configuration action " + pl.Action)
	}
	return nil
}

// ---------------------------------------------------------------- execute

func executeConfigResource(ctx context.Context, kc *kubeClients, pl changePayload) (any, error) {
	cs := kc.cs
	switch pl.Action {
	case "configmap_create", "configmap_update":
		var p configMapParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return nil, err
		}
		if pl.Action == "configmap_create" {
			binaryData, err := decodeConfigBinaryData(p.BinaryData)
			if err != nil {
				return nil, err
			}
			created, err := cs.CoreV1().ConfigMaps(p.Namespace).Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: p.Labels, Annotations: p.Annotations},
				Data: p.Data, BinaryData: binaryData, Immutable: p.Immutable,
			}, metav1.CreateOptions{FieldManager: fieldManager})
			if err != nil {
				return nil, err
			}
			return configMapRow(*created), nil
		}
		current, err := cs.CoreV1().ConfigMaps(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if current.UID != p.UID || current.ResourceVersion != p.ResourceVersion {
			return nil, httpx.Conflict("ConfigMap changed since it was loaded; refresh and retry")
		}
		binaryData, err := decodeConfigBinaryData(p.BinaryData)
		if err != nil {
			return nil, err
		}
		if current.Immutable != nil && *current.Immutable &&
			(!sameStringMap(current.Data, p.Data) || !sameBytesData(current.BinaryData, binaryData)) {
			return nil, httpx.BadRequest("immutable ConfigMap data cannot be changed")
		}
		current.Data = p.Data
		current.BinaryData = binaryData
		if p.Immutable != nil {
			current.Immutable = p.Immutable
		}
		if p.Labels != nil {
			current.Labels = p.Labels
		}
		if p.Annotations != nil {
			current.Annotations = p.Annotations
		}
		updated, err := cs.CoreV1().ConfigMaps(p.Namespace).Update(ctx, current, metav1.UpdateOptions{FieldManager: fieldManager})
		if err != nil {
			return nil, err
		}
		return configMapRow(*updated), nil
	case "configmap_delete":
		uid, rv, err := configMapPreconditions(ctx, cs, pl.Namespace, pl.Name, pl.Params)
		if err != nil {
			return nil, err
		}
		if err := cs.CoreV1().ConfigMaps(pl.Namespace).Delete(ctx, pl.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
		}); err != nil {
			return nil, err
		}
		return gin.H{"namespace": pl.Namespace, "name": pl.Name, "deleted": true}, nil
	case "secret_create", "secret_update":
		var p secretParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return nil, err
		}
		data := map[string][]byte{}
		for k, v := range p.Data {
			data[k] = []byte(v)
		}
		if pl.Action == "secret_create" {
			created, err := cs.CoreV1().Secrets(p.Namespace).Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: p.Labels, Annotations: p.Annotations},
				Type:       corev1.SecretType(p.Type), Data: data, Immutable: p.Immutable,
			}, metav1.CreateOptions{FieldManager: fieldManager})
			if err != nil {
				return nil, err
			}
			return secretRow(*created), nil
		}
		current, err := cs.CoreV1().Secrets(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if current.UID != p.UID || current.ResourceVersion != p.ResourceVersion {
			return nil, httpx.Conflict("Secret changed since it was loaded; refresh and retry")
		}
		if string(current.Type) != p.Type {
			return nil, httpx.BadRequest("Secret type cannot be changed")
		}
		nextData := make(map[string][]byte, len(current.Data)+len(data))
		if p.Merge {
			for key, value := range current.Data {
				nextData[key] = value
			}
			for key, value := range data {
				nextData[key] = value
			}
			for _, key := range p.DeleteKeys {
				delete(nextData, key)
			}
		} else {
			for key, value := range data {
				nextData[key] = value
			}
		}
		if current.Immutable != nil && *current.Immutable && !sameBytesData(current.Data, nextData) {
			return nil, httpx.BadRequest("immutable Secret data cannot be changed")
		}
		size := 0
		for key, value := range nextData {
			size += len(key) + len(value)
		}
		if size > maxConfigBytes {
			return nil, httpx.BadRequest("Secret data exceeds 1 MiB")
		}
		current.Data = nextData
		current.StringData = nil
		if p.Immutable != nil {
			current.Immutable = p.Immutable
		}
		if p.Labels != nil {
			current.Labels = p.Labels
		}
		if p.Annotations != nil {
			current.Annotations = p.Annotations
		}
		updated, err := cs.CoreV1().Secrets(p.Namespace).Update(ctx, current, metav1.UpdateOptions{FieldManager: fieldManager})
		if err != nil {
			return nil, err
		}
		return secretRow(*updated), nil
	case "secret_delete":
		uid, rv, err := secretPreconditions(ctx, cs, pl.Namespace, pl.Name, pl.Params)
		if err != nil {
			return nil, err
		}
		if err := cs.CoreV1().Secrets(pl.Namespace).Delete(ctx, pl.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
		}); err != nil {
			return nil, err
		}
		return gin.H{"namespace": pl.Namespace, "name": pl.Name, "deleted": true}, nil
	case "configmap_batch_delete", "secret_batch_delete":
		var p configBatchParams
		_ = decodeParams(pl.Params, &p)
		results := make([]gin.H, 0, len(p.Items))
		failed := 0
		for _, item := range p.Items {
			var err error
			if pl.Action == "configmap_batch_delete" {
				_, _, err = configMapPreconditions(ctx, cs, item.Namespace, item.Name, mustJSON(item))
				if err == nil {
					err = cs.CoreV1().ConfigMaps(item.Namespace).Delete(ctx, item.Name, metav1.DeleteOptions{
						Preconditions: &metav1.Preconditions{UID: &item.UID, ResourceVersion: &item.ResourceVersion},
					})
				}
			} else {
				_, _, err = secretPreconditions(ctx, cs, item.Namespace, item.Name, mustJSON(item))
				if err == nil {
					err = cs.CoreV1().Secrets(item.Namespace).Delete(ctx, item.Name, metav1.DeleteOptions{
						Preconditions: &metav1.Preconditions{UID: &item.UID, ResourceVersion: &item.ResourceVersion},
					})
				}
			}
			if err != nil {
				failed++
				results = append(results, gin.H{"namespace": item.Namespace, "name": item.Name, "status": "failed", "error": err.Error()})
				continue
			}
			results = append(results, gin.H{"namespace": item.Namespace, "name": item.Name, "status": "deleted"})
		}
		return gin.H{"failed": failed, "total": len(results), "items": results}, nil
	}
	return nil, httpx.BadRequest("unknown configuration action " + pl.Action)
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func configMapPreconditions(ctx context.Context, cs interface {
	CoreV1() coreclient.CoreV1Interface
}, namespace, name string, raw json.RawMessage) (types.UID, string, error) {
	var p struct {
		UID             types.UID `json:"uid"`
		ResourceVersion string    `json:"resource_version"`
	}
	_ = decodeParams(raw, &p)
	if p.UID == "" || p.ResourceVersion == "" {
		return "", "", httpx.BadRequest("uid and resource_version are required")
	}
	cm, err := cs.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	if cm.UID != p.UID || cm.ResourceVersion != p.ResourceVersion {
		return "", "", httpx.Conflict("ConfigMap changed since it was loaded; refresh and retry")
	}
	return p.UID, p.ResourceVersion, nil
}

func secretPreconditions(ctx context.Context, cs interface {
	CoreV1() coreclient.CoreV1Interface
}, namespace, name string, raw json.RawMessage) (types.UID, string, error) {
	var p struct {
		UID             types.UID `json:"uid"`
		ResourceVersion string    `json:"resource_version"`
	}
	_ = decodeParams(raw, &p)
	if p.UID == "" || p.ResourceVersion == "" {
		return "", "", httpx.BadRequest("uid and resource_version are required")
	}
	sec, err := cs.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	if sec.UID != p.UID || sec.ResourceVersion != p.ResourceVersion {
		return "", "", httpx.Conflict("Secret changed since it was loaded; refresh and retry")
	}
	return p.UID, p.ResourceVersion, nil
}
