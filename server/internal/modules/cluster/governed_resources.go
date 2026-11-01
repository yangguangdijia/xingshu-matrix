package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

var governedRBACResources = map[string]schema.GroupVersionResource{
	"serviceaccounts":     {Version: "v1", Resource: "serviceaccounts"},
	"roles":               {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"},
	"clusterroles":        {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
	"rolebindings":        {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
	"clusterrolebindings": {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
}

var governedRBACKinds = map[schema.GroupVersionKind]string{
	{Version: "v1", Kind: "ServiceAccount"}:                                         "serviceaccounts",
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"}:               "roles",
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}:        "clusterroles",
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"}:        "rolebindings",
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"}: "clusterrolebindings",
}

var governedVeleroKinds = map[schema.GroupVersionKind]string{
	{Group: "velero.io", Version: "v1", Kind: "Backup"}:   "backups",
	{Group: "velero.io", Version: "v1", Kind: "Schedule"}: "schedules",
}

type governedYAMLParams struct {
	YAML            string    `json:"yaml"`
	Namespace       string    `json:"namespace,omitempty"`
	UID             types.UID `json:"uid,omitempty"`
	ResourceVersion string    `json:"resource_version,omitempty"`
}

type governedManifest struct {
	object   *unstructured.Unstructured
	resource string
}

func parseGovernedYAML(raw, namespace string, allowed map[schema.GroupVersionKind]string, namespaced map[string]bool) ([]governedManifest, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, httpx.BadRequest("yaml is required")
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(raw)), 4096)
	objects := make([]governedManifest, 0)
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(&obj.Object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, httpx.BadRequest("invalid governed resource YAML")
		}
		if len(obj.Object) == 0 {
			continue
		}
		if obj.GetKind() == "List" {
			return nil, httpx.Forbidden("Kubernetes List manifests are not allowed")
		}
		gvrName, ok := allowed[obj.GroupVersionKind()]
		if !ok {
			return nil, httpx.Forbidden("apiVersion/kind is not allowed for this endpoint")
		}
		if obj.GetName() == "" {
			return nil, httpx.BadRequest("metadata.name is required")
		}
		if errs := validation.IsDNS1123Subdomain(obj.GetName()); len(errs) > 0 {
			return nil, httpx.BadRequest("invalid metadata.name")
		}
		isNamespaced := namespaced[gvrName]
		if isNamespaced {
			if obj.GetNamespace() == "" {
				obj.SetNamespace(namespace)
			}
			if obj.GetNamespace() == "" {
				return nil, httpx.BadRequest("metadata.namespace or namespace is required for namespaced objects")
			}
			if errs := validation.IsDNS1123Label(obj.GetNamespace()); len(errs) > 0 {
				return nil, httpx.BadRequest("invalid metadata.namespace")
			}
		} else if obj.GetNamespace() != "" {
			return nil, httpx.BadRequest("cluster-scoped resources must not set metadata.namespace")
		}
		obj.SetManagedFields(nil)
		objects = append(objects, governedManifest{object: obj, resource: gvrName})
		if len(objects) > 100 {
			return nil, httpx.BadRequest("at most 100 YAML documents are allowed")
		}
	}
	if len(objects) == 0 {
		return nil, httpx.BadRequest("no objects in yaml")
	}
	return objects, nil
}

func governedManifests(raw, namespace string, velero bool) ([]governedManifest, error) {
	if velero {
		return parseGovernedYAML(raw, namespace, governedVeleroKinds, map[string]bool{"backups": true, "schedules": true})
	}
	return parseGovernedYAML(raw, namespace, governedRBACKinds, map[string]bool{
		"serviceaccounts": true, "roles": true, "rolebindings": true,
	})
}

// Missing identity means create-only. Updates carry their reviewed identity in
// each document so delayed approvals cannot silently switch targets or modes.
func governedApplyManifests(params governedYAMLParams, velero bool) ([]governedManifest, error) {
	objects, err := governedManifests(params.YAML, params.Namespace, velero)
	if err != nil {
		return nil, err
	}
	if params.UID != "" || params.ResourceVersion != "" {
		if len(objects) != 1 || params.UID == "" || params.ResourceVersion == "" {
			return nil, httpx.BadRequest("top-level uid and resource_version require exactly one update document")
		}
	}
	seen := make(map[string]bool, len(objects))
	for _, item := range objects {
		obj := item.object
		uid, _, uidErr := unstructured.NestedString(obj.Object, "metadata", "uid")
		rv, _, rvErr := unstructured.NestedString(obj.Object, "metadata", "resourceVersion")
		if uidErr != nil || rvErr != nil {
			return nil, httpx.BadRequest("resource identity preconditions must be strings")
		}
		if params.UID != "" {
			if (uid != "" && uid != string(params.UID)) || (rv != "" && rv != params.ResourceVersion) {
				return nil, httpx.BadRequest("top-level and manifest identity preconditions disagree")
			}
			uid, rv = string(params.UID), params.ResourceVersion
			obj.SetUID(params.UID)
			obj.SetResourceVersion(rv)
		}
		if (uid == "") != (rv == "") || len(uid) > 128 || len(rv) > 128 ||
			strings.TrimSpace(uid) != uid || strings.TrimSpace(rv) != rv {
			return nil, httpx.BadRequest("updates require both valid uid and resourceVersion; creates must omit both")
		}
		identity := obj.GroupVersionKind().Group + "/" + item.resource + "/" + obj.GetNamespace() + "/" + obj.GetName()
		if seen[identity] {
			return nil, httpx.BadRequest("duplicate resource identity in governed YAML")
		}
		seen[identity] = true
	}
	return objects, nil
}

func validateGovernedChange(pl *changePayload) error {
	switch pl.Action {
	case "rbac_apply", "backup_apply":
		var params governedYAMLParams
		if err := decodeParams(pl.Params, &params); err != nil {
			return err
		}
		velero := pl.Action == "backup_apply"
		objects, err := governedApplyManifests(params, velero)
		if err != nil {
			return err
		}
		documents := make([]string, 0, len(objects))
		for _, item := range objects {
			if velero && item.object.GetKind() == "Schedule" {
				schedule, _, _ := unstructured.NestedString(item.object.Object, "spec", "schedule")
				if strings.TrimSpace(schedule) == "" {
					return httpx.BadRequest("Velero Schedule spec.schedule is required")
				}
			}
			document, err := yaml.Marshal(item.object.Object)
			if err != nil {
				return httpx.BadRequest("invalid governed resource configuration")
			}
			documents = append(documents, string(document))
		}
		params.YAML = strings.Join(documents, "---\n")
		normalized, err := json.Marshal(params)
		if err != nil {
			return httpx.BadRequest("invalid governed YAML params")
		}
		pl.Params = normalized
		return nil
	case "rbac_delete":
		if err := validateRBACIdentity(pl); err != nil {
			return err
		}
		return validateResourcePreconditions(pl.Params, "RBAC resource")
	default:
		return nil
	}
}

func executeGovernedResourceChange(ctx context.Context, kc *kubeClients, pl changePayload) (any, error) {
	if err := validateGovernedChange(&pl); err != nil {
		return nil, err
	}
	switch pl.Action {
	case "rbac_apply":
		var params governedYAMLParams
		_ = decodeParams(pl.Params, &params)
		return applyGovernedYAML(ctx, kc, params, map[string]bool{
			"serviceaccounts": true, "roles": true, "rolebindings": true,
		}, false)
	case "backup_apply":
		if err := veleroAPIsAvailable(ctx, kc); err != nil {
			return nil, err
		}
		var params governedYAMLParams
		_ = decodeParams(pl.Params, &params)
		return applyGovernedYAML(ctx, kc, params, map[string]bool{"backups": true, "schedules": true}, true)
	case "rbac_delete":
		return deleteRBACResource(ctx, kc, pl)
	default:
		return nil, httpx.BadRequest("unknown governed resource action")
	}
}

type governedApplyTarget struct {
	manifest governedManifest
	client   dynamic.ResourceInterface
}

type governedBatchResult struct {
	Objects  []AppliedObject `json:"objects"`
	Applied  int             `json:"applied"`
	Failed   int             `json:"failed"`
	Unknown  int             `json:"unknown"`
	Skipped  int             `json:"skipped"`
	Complete bool            `json:"complete"`
	State    string          `json:"state"`
	Phase    string          `json:"phase"`
}

func governedIdentityCheck(ctx context.Context, target governedApplyTarget) error {
	obj := target.manifest.object
	current, err := target.client.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if obj.GetUID() == "" {
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return resourceAPIError(err)
		}
		if current == nil {
			return resourceUnavailable()
		}
		return httpx.Conflict("create-only governed resource already exists; reload and submit an explicit update")
	}
	if apierrors.IsNotFound(err) {
		return httpx.Conflict("governed update target no longer exists; submit a new create configuration")
	}
	if err != nil {
		return resourceAPIError(err)
	}
	if current == nil {
		return resourceUnavailable()
	}
	if current.GetUID() != obj.GetUID() || current.GetResourceVersion() != obj.GetResourceVersion() {
		return httpx.Conflict("governed resource identity changed; reload and submit a new approval")
	}
	return nil
}

func writeGovernedTarget(ctx context.Context, target governedApplyTarget, dryRun bool) (*unstructured.Unstructured, error) {
	obj := target.manifest.object.DeepCopy()
	if obj.GetUID() == "" {
		opts := metav1.CreateOptions{FieldManager: fieldManager}
		if dryRun {
			opts.DryRun = []string{metav1.DryRunAll}
		}
		// Create is atomic absence enforcement; an apply could overwrite an
		// object created between the identity read and this write.
		return target.client.Create(ctx, obj, opts)
	}
	opts := metav1.UpdateOptions{FieldManager: fieldManager}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	// Update carries the reviewed resourceVersion as an atomic optimistic-lock precondition.
	return target.client.Update(ctx, obj, opts)
}

func governedWriteUncertain(err error) bool {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		code := status.Status().Code
		return code == http.StatusRequestTimeout || code >= http.StatusInternalServerError || code == 0
	}
	return true // A transport failure can occur after the API committed a write.
}

func governedBatchFailure(result *governedBatchResult, index int, err error, uncertain bool) (any, error) {
	var platform *httpx.Error
	if !errors.As(resourceAPIError(err), &platform) {
		platform = httpx.NewError(http.StatusBadGateway, httpx.CodeUpstream, "governed resource operation failed")
	}
	result.Objects[index].Status = "failed"
	result.Failed = 1
	if uncertain {
		result.Objects[index].Status = "unknown"
		result.Failed, result.Unknown = 0, 1
	}
	result.Objects[index].Error = platform.Message
	result.Skipped = len(result.Objects) - result.Applied - result.Failed - result.Unknown
	result.State = "rejected"
	if result.Unknown > 0 {
		result.State = "uncertain"
	}
	if result.Applied > 0 {
		result.State = "partial"
	}
	// Shared HTTP execution discards results on error; approval records persist
	// err.Error(). Keep the redacted outcome in both channels without claiming rollback.
	summary, _ := json.Marshal(result)
	failure := httpx.NewError(platform.Status, platform.Code,
		"governed resource batch did not complete; no rollback performed; outcome="+string(summary)).WithDetails(result)
	return result, failure
}

func applyGovernedYAML(ctx context.Context, kc *kubeClients, params governedYAMLParams, namespaced map[string]bool, velero bool) (any, error) {
	objects, err := governedApplyManifests(params, velero)
	if err != nil {
		return nil, err
	}
	if kc == nil || kc.mapper == nil || kc.dyn == nil {
		return nil, resourceUnavailable()
	}
	result := &governedBatchResult{Objects: make([]AppliedObject, len(objects)), Phase: "preflight"}
	for i, item := range objects {
		obj := item.object
		result.Objects[i] = AppliedObject{APIVersion: obj.GetAPIVersion(), Kind: obj.GetKind(),
			Namespace: obj.GetNamespace(), Name: obj.GetName(), Status: "skipped"}
	}
	targets := make([]governedApplyTarget, 0, len(objects))
	for i, item := range objects {
		mapping, err := kc.mapper.RESTMapping(item.object.GroupVersionKind().GroupKind(), item.object.GroupVersionKind().Version)
		if err != nil {
			kc.mapper.Reset()
			return governedBatchFailure(result, i, resourceUnavailable(), false)
		}
		if mapping.Resource.Resource != item.resource ||
			(mapping.Scope.Name() == "namespace") != namespaced[item.resource] {
			return governedBatchFailure(result, i, httpx.Forbidden("resource scope does not match the allowed API kind"), false)
		}
		var client dynamic.ResourceInterface = kc.dyn.Resource(mapping.Resource)
		if namespaced[item.resource] {
			client = kc.dyn.Resource(mapping.Resource).Namespace(item.object.GetNamespace())
		}
		targets = append(targets, governedApplyTarget{manifest: item, client: client})
	}
	// Resolve and validate the complete batch before allowing its first write.
	for i, target := range targets {
		if err := governedIdentityCheck(ctx, target); err != nil {
			return governedBatchFailure(result, i, err, false)
		}
		preview, err := writeGovernedTarget(ctx, target, true)
		if err != nil {
			return governedBatchFailure(result, i, err, false)
		}
		if preview == nil {
			return governedBatchFailure(result, i, resourceUnavailable(), false)
		}
	}
	result.Phase = "execution"
	for i, target := range targets {
		if err := governedIdentityCheck(ctx, target); err != nil {
			return governedBatchFailure(result, i, err, false)
		}
		applied, err := writeGovernedTarget(ctx, target, false)
		if err != nil {
			return governedBatchFailure(result, i, err, governedWriteUncertain(err))
		}
		if applied == nil {
			return governedBatchFailure(result, i, resourceUnavailable(), true)
		}
		result.Objects[i].Status = "applied"
		result.Applied++
	}
	result.Complete, result.State = true, "applied"
	return result, nil
}

func rbacList(ctx context.Context, kc *kubeClients, namespace string) (map[string]ClusterCapability, error) {
	out := make(map[string]ClusterCapability, len(governedRBACResources))
	for key, gvr := range governedRBACResources {
		ns := ""
		if gvr.Resource == "serviceaccounts" || gvr.Resource == "roles" || gvr.Resource == "rolebindings" {
			ns = namespace
		}
		item := capabilityResource(ctx, kc, gvr, ns)
		out[key] = item
		if !item.Available && item.State != "missing" {
			return out, kubeErrFromCapability(item)
		}
	}
	return out, nil
}

func kubeErrFromCapability(cap ClusterCapability) error {
	switch cap.State {
	case "forbidden":
		return httpx.NewError(403, httpx.CodeForbidden, cap.Error)
	case "missing":
		return httpx.NotFound(cap.Error)
	default:
		return httpx.NewError(502, httpx.CodeUpstream, cap.Error)
	}
}

func (m *Module) listRBAC(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	items, err := rbacList(c.Request.Context(), kc, c.Query("namespace"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"resources": items})
}

func (m *Module) getRBACYAML(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	pl := changePayload{Kind: c.Param("kind"), Namespace: c.Param("ns"), Name: c.Param("name")}
	if err := validateRBACIdentity(&pl); err != nil {
		httpx.Fail(c, err)
		return
	}
	resource := governedRBACResources[pl.Kind]
	var item *unstructured.Unstructured
	var err error
	if pl.Namespace == "" {
		item, err = kc.dyn.Resource(resource).Get(c.Request.Context(), pl.Name, metav1.GetOptions{})
	} else {
		item, err = kc.dyn.Resource(resource).Namespace(pl.Namespace).Get(c.Request.Context(), pl.Name, metav1.GetOptions{})
	}
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	item.SetManagedFields(nil)
	raw, err := json.Marshal(item.Object)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	out, err := yaml.JSONToYAML(raw)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	httpx.OK(c, gin.H{"kind": item.GetKind(), "namespace": item.GetNamespace(), "name": item.GetName(), "yaml": string(out)})
}

func validateRBACIdentity(pl *changePayload) error {
	resource, ok := rbacResourceForKind(pl.Kind)
	if !ok {
		return httpx.Forbidden("unsupported RBAC kind")
	}
	pl.Kind = resource.Resource
	if resource.Resource == "serviceaccounts" || resource.Resource == "roles" || resource.Resource == "rolebindings" {
		if len(validation.IsDNS1123Label(pl.Namespace)) != 0 {
			return httpx.BadRequest("namespaced RBAC resource requires a valid namespace")
		}
	} else if pl.Namespace != "-" && pl.Namespace != "" {
		return httpx.BadRequest("cluster-scoped RBAC resource requires namespace '-'")
	} else {
		pl.Namespace = ""
	}
	if pl.Name == "" || len(validation.IsDNS1123Subdomain(pl.Name)) > 0 {
		return httpx.BadRequest("invalid RBAC resource name")
	}
	return nil
}

func rbacResourceForKind(kind string) (schema.GroupVersionResource, bool) {
	switch strings.ToLower(kind) {
	case "serviceaccount", "serviceaccounts":
		return governedRBACResources["serviceaccounts"], true
	case "role", "roles":
		return governedRBACResources["roles"], true
	case "clusterrole", "clusterroles":
		return governedRBACResources["clusterroles"], true
	case "rolebinding", "rolebindings":
		return governedRBACResources["rolebindings"], true
	case "clusterrolebinding", "clusterrolebindings":
		return governedRBACResources["clusterrolebindings"], true
	default:
		return schema.GroupVersionResource{}, false
	}
}

func deleteRBACResource(ctx context.Context, kc *kubeClients, pl changePayload) (any, error) {
	if err := validateRBACIdentity(&pl); err != nil {
		return nil, err
	}
	if err := validateResourcePreconditions(pl.Params, "RBAC resource"); err != nil {
		return nil, err
	}
	if kc == nil || kc.dyn == nil {
		return nil, resourceUnavailable()
	}
	var preconditions struct {
		UID             types.UID `json:"uid"`
		ResourceVersion string    `json:"resource_version"`
	}
	if err := decodeParams(pl.Params, &preconditions); err != nil {
		return nil, err
	}
	uid, rv := preconditions.UID, preconditions.ResourceVersion
	if pl.Namespace == "" {
		err := kc.dyn.Resource(governedRBACResources[pl.Kind]).Delete(ctx, pl.Name,
			metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
		if err != nil {
			return nil, err
		}
	} else {
		err := kc.dyn.Resource(governedRBACResources[pl.Kind]).Namespace(pl.Namespace).Delete(ctx, pl.Name,
			metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
		if err != nil {
			return nil, err
		}
	}
	return gin.H{"kind": pl.Kind, "namespace": pl.Namespace, "name": pl.Name, "deleted": true}, nil
}

func backupResource(ctx context.Context, kc *kubeClients, key, namespace string) (ClusterCapability, error) {
	gvr := capabilityResources[key]
	item := capabilityResource(ctx, kc, gvr, namespace)
	if item.State == "forbidden" || item.State == "error" {
		return item, kubeErrFromCapability(item)
	}
	return item, nil
}

func (m *Module) listBackups(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	ctx, namespace := c.Request.Context(), c.Query("namespace")
	result := map[string]ClusterCapability{}
	for _, key := range []string{"velero_backups", "velero_schedules", "velero_storage_locations"} {
		item, err := backupResource(ctx, kc, key, namespace)
		result[key] = item
		if err != nil {
			httpx.Fail(c, err)
			return
		}
	}
	capability := snapshotCapability(ctx, kc)
	snapshots := ClusterCapability{Available: capability.Supported, Items: []CapabilityItem{}}
	if capability.Supported {
		snapshots = capabilityResource(ctx, kc, snapshotGVR, namespace)
	} else {
		snapshots = emptyCapability("missing", capability.Reason)
	}
	result["volume_snapshots"] = snapshots
	httpx.OK(c, gin.H{"resources": result, "velero": veleroCapability(result), "snapshots": capability})
}
