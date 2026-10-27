package cluster

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// Storage resources are part of the cluster module: they share the cluster
// registry, client cache, environment scope and the cluster.change approval
// gateway. No second cluster registry or direct write path is introduced.

const (
	PermStorageRead  = "storage:read"
	PermStorageWrite = "storage:write"

	snapshotGroup = "snapshot.storage.k8s.io"
)

var (
	snapshotClassGVR = schema.GroupVersionResource{Group: snapshotGroup, Version: "v1", Resource: "volumesnapshotclasses"}
	snapshotGVR      = schema.GroupVersionResource{Group: snapshotGroup, Version: "v1", Resource: "volumesnapshots"}
)

// ---------------------------------------------------------------- read models

// StorageClassRow is a StorageClass summary. UID and ResourceVersion are
// included because the update flow checks the loaded resource version.
type StorageClassRow struct {
	Name                 string            `json:"name"`
	UID                  types.UID         `json:"uid,omitempty"`
	ResourceVersion      string            `json:"resource_version,omitempty"`
	Provisioner          string            `json:"provisioner"`
	ReclaimPolicy        string            `json:"reclaim_policy"`
	VolumeBindingMode    string            `json:"volume_binding_mode"`
	AllowVolumeExpansion bool              `json:"allow_volume_expansion"`
	Default              bool              `json:"default"`
	Parameters           map[string]string `json:"parameters"`
	MountOptions         []string          `json:"mount_options"`
	CreatedAt            metav1.Time       `json:"created_at"`
}

// PersistentVolumeRow is a PersistentVolume summary. The source fields let the
// edit flow resend an immutable source, and UID/ResourceVersion let delete use
// preconditions.
type PersistentVolumeRow struct {
	Name            string                              `json:"name"`
	UID             types.UID                           `json:"uid,omitempty"`
	ResourceVersion string                              `json:"resource_version,omitempty"`
	Phase           string                              `json:"phase"`
	Capacity        string                              `json:"capacity"`
	AccessModes     []corev1.PersistentVolumeAccessMode `json:"access_modes"`
	ReclaimPolicy   string                              `json:"reclaim_policy"`
	StorageClass    string                              `json:"storage_class"`
	ClaimRef        string                              `json:"claim_ref,omitempty"`
	VolumeMode      string                              `json:"volume_mode"`
	HostPath        string                              `json:"host_path,omitempty"`
	NFS             *pvNFSParams                        `json:"nfs,omitempty"`
	Labels          map[string]string                   `json:"labels"`
	Annotations     map[string]string                   `json:"annotations"`
	CreatedAt       metav1.Time                         `json:"created_at"`
}

// PersistentVolumeClaimRow is a PersistentVolumeClaim summary. UID and
// ResourceVersion are required by update/delete conflict checks.
type PersistentVolumeClaimRow struct {
	Namespace       string                              `json:"namespace"`
	Name            string                              `json:"name"`
	UID             types.UID                           `json:"uid,omitempty"`
	ResourceVersion string                              `json:"resource_version,omitempty"`
	Phase           string                              `json:"phase"`
	Requested       string                              `json:"requested"`
	Capacity        string                              `json:"capacity"`
	AccessModes     []corev1.PersistentVolumeAccessMode `json:"access_modes"`
	StorageClass    string                              `json:"storage_class"`
	VolumeName      string                              `json:"volume_name"`
	VolumeMode      string                              `json:"volume_mode"`
	Labels          map[string]string                   `json:"labels"`
	Annotations     map[string]string                   `json:"annotations"`
	CreatedAt       metav1.Time                         `json:"created_at"`
}

// VolumeSnapshotRow is a VolumeSnapshot summary. It is only filled when the
// snapshot API and controller resources actually exist. UID and ResourceVersion
// are required by the delete precondition.
type VolumeSnapshotRow struct {
	Namespace       string            `json:"namespace"`
	Name            string            `json:"name"`
	UID             types.UID         `json:"uid,omitempty"`
	ResourceVersion string            `json:"resource_version,omitempty"`
	Class           string            `json:"class"`
	SourcePVC       string            `json:"source_pvc"`
	Ready           bool              `json:"ready"`
	RestoreSize     string            `json:"restore_size"`
	Error           string            `json:"error,omitempty"`
	Labels          map[string]string `json:"labels"`
	CreatedAt       metav1.Time       `json:"created_at"`
}

// SnapshotCapability describes whether snapshot CRUD can be offered. It is
// derived from discovered APIs and resources; a missing controller is never
// reported as success.
type SnapshotCapability struct {
	Supported bool     `json:"supported"`
	Reason    string   `json:"reason,omitempty"`
	Classes   []string `json:"classes"`
}

func storageClassRow(sc storagev1.StorageClass) StorageClassRow {
	reclaim := string(corev1.PersistentVolumeReclaimDelete)
	if sc.ReclaimPolicy != nil {
		reclaim = string(*sc.ReclaimPolicy)
	}
	binding := string(storagev1.VolumeBindingImmediate)
	if sc.VolumeBindingMode != nil {
		binding = string(*sc.VolumeBindingMode)
	}
	allow := sc.AllowVolumeExpansion != nil && *sc.AllowVolumeExpansion
	return StorageClassRow{Name: sc.Name, UID: sc.UID, ResourceVersion: sc.ResourceVersion,
		Provisioner: sc.Provisioner, ReclaimPolicy: reclaim, VolumeBindingMode: binding,
		AllowVolumeExpansion: allow, Default: isDefaultStorageClass(sc), Parameters: sc.Parameters,
		MountOptions: sc.MountOptions, CreatedAt: sc.CreationTimestamp}
}

func isDefaultStorageClass(sc storagev1.StorageClass) bool {
	for _, key := range []string{"storageclass.kubernetes.io/is-default-class", "storageclass.beta.kubernetes.io/is-default-class"} {
		if strings.EqualFold(sc.Annotations[key], "true") {
			return true
		}
	}
	return false
}

func pvRow(pv corev1.PersistentVolume) PersistentVolumeRow {
	claim := ""
	if pv.Spec.ClaimRef != nil {
		claim = pv.Spec.ClaimRef.Namespace + "/" + pv.Spec.ClaimRef.Name
	}
	mode := string(corev1.PersistentVolumeFilesystem)
	if pv.Spec.VolumeMode != nil {
		mode = string(*pv.Spec.VolumeMode)
	}
	row := PersistentVolumeRow{Name: pv.Name, UID: pv.UID, ResourceVersion: pv.ResourceVersion,
		Phase: string(pv.Status.Phase), Capacity: pv.Spec.Capacity.Storage().String(),
		AccessModes: pv.Spec.AccessModes, ReclaimPolicy: string(pv.Spec.PersistentVolumeReclaimPolicy),
		StorageClass: pv.Spec.StorageClassName, ClaimRef: claim, VolumeMode: mode, Labels: pv.Labels,
		Annotations: pv.Annotations, CreatedAt: pv.CreationTimestamp}
	if pv.Spec.HostPath != nil {
		row.HostPath = pv.Spec.HostPath.Path
	}
	if pv.Spec.NFS != nil {
		row.NFS = &pvNFSParams{Server: pv.Spec.NFS.Server, Path: pv.Spec.NFS.Path, ReadOnly: pv.Spec.NFS.ReadOnly}
	}
	return row
}

func pvcRow(pvc corev1.PersistentVolumeClaim) PersistentVolumeClaimRow {
	mode := string(corev1.PersistentVolumeFilesystem)
	if pvc.Spec.VolumeMode != nil {
		mode = string(*pvc.Spec.VolumeMode)
	}
	requested := ""
	if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		requested = q.String()
	}
	capacity := ""
	if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		capacity = q.String()
	}
	class := ""
	if pvc.Spec.StorageClassName != nil {
		class = *pvc.Spec.StorageClassName
	}
	return PersistentVolumeClaimRow{Namespace: pvc.Namespace, Name: pvc.Name, UID: pvc.UID,
		ResourceVersion: pvc.ResourceVersion, Phase: string(pvc.Status.Phase),
		Requested: requested, Capacity: capacity, AccessModes: pvc.Spec.AccessModes, StorageClass: class,
		VolumeName: pvc.Spec.VolumeName, VolumeMode: mode, Labels: pvc.Labels, Annotations: pvc.Annotations,
		CreatedAt: pvc.CreationTimestamp}
}

func snapshotRow(obj unstructured.Unstructured) VolumeSnapshotRow {
	obj.SetManagedFields(nil)
	spec, _ := obj.Object["spec"].(map[string]any)
	status, _ := obj.Object["status"].(map[string]any)
	row := VolumeSnapshotRow{Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: obj.GetUID(),
		ResourceVersion: obj.GetResourceVersion(), Labels: obj.GetLabels(), CreatedAt: obj.GetCreationTimestamp()}
	if spec != nil {
		row.Class, _ = spec["volumeSnapshotClassName"].(string)
		if source, ok := spec["source"].(map[string]any); ok {
			row.SourcePVC, _ = source["persistentVolumeClaimName"].(string)
		}
	}
	if status != nil {
		row.Ready, _ = status["readyToUse"].(bool)
		if em, ok := status["error"].(map[string]any); ok {
			row.Error, _ = em["message"].(string)
		}
		if restore, ok := status["restoreSize"].(string); ok {
			row.RestoreSize = restore
		}
	}
	return row
}

// ---------------------------------------------------------------- capability

// snapshotCapability probes the snapshot API and its controller resources. A
// CRD alone is not enough: without a VolumeSnapshotClass or a CSI driver the
// platform must keep snapshot actions disabled and explain why.
func snapshotCapability(ctx context.Context, kc *kubeClients) SnapshotCapability {
	out := SnapshotCapability{Classes: []string{}}
	if kc == nil || kc.cs == nil {
		out.Reason = "集群客户端不可用"
		return out
	}
	discovery, err := kc.cs.Discovery().ServerResourcesForGroupVersion(snapshotGroup + "/v1")
	if err != nil {
		if apierrors.IsNotFound(err) || strings.Contains(strings.ToLower(err.Error()), "not found") {
			out.Reason = "集群未安装 snapshot.storage.k8s.io/v1 CRD"
		} else {
			out.Reason = "无法发现 snapshot.storage.k8s.io/v1 API：" + err.Error()
		}
		return out
	}
	hasSnapshots, hasClasses := false, false
	for _, r := range discovery.APIResources {
		switch r.Name {
		case "volumesnapshots":
			hasSnapshots = true
		case "volumesnapshotclasses":
			hasClasses = true
		}
	}
	if !hasSnapshots || !hasClasses {
		out.Reason = "snapshot API 不完整（缺少 VolumeSnapshot 或 VolumeSnapshotClass）"
		return out
	}
	listCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	classes, err := kc.dyn.Resource(snapshotClassGVR).List(listCtx, metav1.ListOptions{Limit: 500})
	if err != nil {
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			out.Reason = "集群凭据无权读取 VolumeSnapshotClass"
		} else {
			out.Reason = "读取 VolumeSnapshotClass 失败：" + err.Error()
		}
		return out
	}
	for _, item := range classes.Items {
		out.Classes = append(out.Classes, item.GetName())
	}
	sort.Strings(out.Classes)
	if len(out.Classes) == 0 {
		out.Reason = "未发现 VolumeSnapshotClass（snapshot controller 未配置存储驱动）"
		return out
	}
	drivers, err := kc.cs.StorageV1().CSIDrivers().List(listCtx, metav1.ListOptions{Limit: 1})
	if err != nil {
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			out.Reason = "集群凭据无权读取 CSIDriver，无法确认 CSI 快照能力"
		} else {
			out.Reason = "读取 CSIDriver 失败：" + err.Error()
		}
		return out
	}
	if len(drivers.Items) == 0 {
		out.Reason = "未发现 CSIDriver，无法确认 CSI 快照能力"
		return out
	}
	out.Supported = true
	return out
}

// ---------------------------------------------------------------- read handlers

func (m *Module) listStorageClasses(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	list, err := kc.cs.StorageV1().StorageClasses().List(c.Request.Context(), metav1.ListOptions{})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	items := make([]StorageClassRow, 0, len(list.Items))
	for _, sc := range list.Items {
		items = append(items, storageClassRow(sc))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	httpx.OK(c, gin.H{"items": items, "total": len(items)})
}

func (m *Module) listPersistentVolumes(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	list, err := kc.cs.CoreV1().PersistentVolumes().List(c.Request.Context(), metav1.ListOptions{})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	phase, class, keyword := c.Query("phase"), c.Query("storage_class"), strings.ToLower(strings.TrimSpace(c.Query("keyword")))
	items := make([]PersistentVolumeRow, 0, len(list.Items))
	for _, pv := range list.Items {
		row := pvRow(pv)
		if phase != "" && row.Phase != phase {
			continue
		}
		if class != "" && row.StorageClass != class {
			continue
		}
		if keyword != "" && !strings.Contains(strings.ToLower(row.Name+" "+row.ClaimRef+" "+row.StorageClass), keyword) {
			continue
		}
		items = append(items, row)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	httpx.OK(c, gin.H{"items": items, "total": len(items)})
}

func (m *Module) listPersistentVolumeClaims(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	ns := c.Query("namespace")
	list, err := kc.cs.CoreV1().PersistentVolumeClaims(ns).List(c.Request.Context(), metav1.ListOptions{})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	phase, class, keyword := c.Query("phase"), c.Query("storage_class"), strings.ToLower(strings.TrimSpace(c.Query("keyword")))
	items := make([]PersistentVolumeClaimRow, 0, len(list.Items))
	for _, pvc := range list.Items {
		row := pvcRow(pvc)
		if phase != "" && row.Phase != phase {
			continue
		}
		if class != "" && row.StorageClass != class {
			continue
		}
		if keyword != "" && !strings.Contains(strings.ToLower(row.Namespace+"/"+row.Name+" "+row.VolumeName+" "+row.StorageClass), keyword) {
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

func (m *Module) storageCapabilities(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	httpx.OK(c, gin.H{"snapshots": snapshotCapability(c.Request.Context(), kc)})
}

func (m *Module) listVolumeSnapshots(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	capability := snapshotCapability(c.Request.Context(), kc)
	if !capability.Supported {
		httpx.OK(c, gin.H{"items": []VolumeSnapshotRow{}, "total": 0, "capability": capability})
		return
	}
	list, err := kc.dyn.Resource(snapshotGVR).Namespace(c.Query("namespace")).List(c.Request.Context(), metav1.ListOptions{})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	items := make([]VolumeSnapshotRow, 0, len(list.Items))
	for _, item := range list.Items {
		items = append(items, snapshotRow(item))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace == items[j].Namespace {
			return items[i].Name < items[j].Name
		}
		return items[i].Namespace < items[j].Namespace
	})
	httpx.OK(c, gin.H{"items": items, "total": len(items), "capability": capability})
}

// ---------------------------------------------------------------- payloads

type storageClassParams struct {
	Name                 string            `json:"name"`
	UID                  types.UID         `json:"uid"`
	Provisioner          string            `json:"provisioner"`
	ReclaimPolicy        string            `json:"reclaim_policy"`
	VolumeBindingMode    string            `json:"volume_binding_mode"`
	AllowVolumeExpansion *bool             `json:"allow_volume_expansion"`
	Parameters           map[string]string `json:"parameters"`
	MountOptions         []string          `json:"mount_options"`
	IsDefault            *bool             `json:"is_default"`
	ResourceVersion      string            `json:"resource_version"`
	Force                bool              `json:"force"`
}

type pvcParams struct {
	Name            string                              `json:"name"`
	Namespace       string                              `json:"namespace"`
	StorageClass    string                              `json:"storage_class"`
	SnapshotName    string                              `json:"snapshot_name,omitempty"`
	AccessModes     []corev1.PersistentVolumeAccessMode `json:"access_modes"`
	Size            string                              `json:"size"`
	VolumeMode      string                              `json:"volume_mode"`
	VolumeName      string                              `json:"volume_name"`
	Labels          map[string]string                   `json:"labels"`
	ResourceVersion string                              `json:"resource_version"`
	UID             types.UID                           `json:"uid"`
	Confirm         string                              `json:"confirm"`
	AllowReclaimDel bool                                `json:"allow_reclaim_delete"`
}

type pvcBatchItem struct {
	Namespace       string    `json:"namespace"`
	Name            string    `json:"name"`
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resource_version"`
	AllowReclaimDel bool      `json:"allow_reclaim_delete"`
}

type pvcBatchParams struct {
	Items   []pvcBatchItem `json:"items"`
	Confirm string         `json:"confirm"`
}

type pvParams struct {
	Name            string                              `json:"name"`
	Capacity        string                              `json:"capacity"`
	AccessModes     []corev1.PersistentVolumeAccessMode `json:"access_modes"`
	ReclaimPolicy   string                              `json:"reclaim_policy"`
	StorageClass    string                              `json:"storage_class"`
	VolumeMode      string                              `json:"volume_mode"`
	HostPath        string                              `json:"host_path"`
	NFS             *pvNFSParams                        `json:"nfs"`
	Labels          map[string]string                   `json:"labels"`
	UID             types.UID                           `json:"uid"`
	ResourceVersion string                              `json:"resource_version"`
	Confirm         string                              `json:"confirm"`
	Force           bool                                `json:"force"`
}

type pvNFSParams struct {
	Server   string `json:"server"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only"`
}

type snapshotParams struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	SourcePVC       string            `json:"source_pvc"`
	ClassName       string            `json:"class_name"`
	Labels          map[string]string `json:"labels"`
	UID             types.UID         `json:"uid"`
	ResourceVersion string            `json:"resource_version"`
}

func validatePVCIdentity(namespace, name string) error {
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		return httpx.BadRequest("invalid namespace")
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return httpx.BadRequest("invalid PVC name")
	}
	return nil
}

func validAccessModes(modes []corev1.PersistentVolumeAccessMode) bool {
	if len(modes) == 0 {
		return false
	}
	seen := map[corev1.PersistentVolumeAccessMode]bool{}
	for _, mode := range modes {
		switch mode {
		case corev1.ReadWriteOnce, corev1.ReadOnlyMany, corev1.ReadWriteMany, corev1.ReadWriteOncePod:
		default:
			return false
		}
		if seen[mode] {
			return false
		}
		seen[mode] = true
	}
	return true
}

func parseStorageSize(size string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(strings.TrimSpace(size))
	if err != nil || q.Sign() <= 0 {
		return q, httpx.BadRequest("size must be a positive Kubernetes quantity, e.g. 10Gi")
	}
	return q, nil
}

func validateStorageClassParams(p *storageClassParams, creating bool) error {
	p.Name = strings.TrimSpace(p.Name)
	if errs := validation.IsDNS1123Subdomain(p.Name); len(errs) > 0 {
		return httpx.BadRequest("invalid StorageClass name")
	}
	if strings.TrimSpace(p.Provisioner) == "" || len(p.Provisioner) > 253 {
		return httpx.BadRequest("provisioner is required")
	}
	if p.ReclaimPolicy != "" && p.ReclaimPolicy != string(corev1.PersistentVolumeReclaimDelete) && p.ReclaimPolicy != string(corev1.PersistentVolumeReclaimRetain) {
		return httpx.BadRequest("reclaim_policy must be Delete or Retain")
	}
	if p.VolumeBindingMode != "" && p.VolumeBindingMode != string(storagev1.VolumeBindingImmediate) && p.VolumeBindingMode != string(storagev1.VolumeBindingWaitForFirstConsumer) {
		return httpx.BadRequest("volume_binding_mode must be Immediate or WaitForFirstConsumer")
	}
	for key := range p.Parameters {
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			return httpx.BadRequest("invalid StorageClass parameter key " + key)
		}
	}
	if !creating && p.ResourceVersion == "" {
		return httpx.BadRequest("resource_version is required for StorageClass update")
	}
	return nil
}

func validateStorageChange(pl *changePayload) error {
	switch pl.Action {
	case "sc_create", "sc_update":
		var p storageClassParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if err := validateStorageClassParams(&p, pl.Action == "sc_create"); err != nil {
			return err
		}
		if pl.Action == "sc_update" {
			p.Name = pl.Name
			if p.ResourceVersion == "" {
				return httpx.BadRequest("resource_version is required for StorageClass update")
			}
		}
		pl.Name, pl.Namespace = p.Name, ""
		normalized, _ := json.Marshal(p)
		pl.Params = normalized
	case "sc_delete":
		if errs := validation.IsDNS1123Subdomain(pl.Name); len(errs) > 0 {
			return httpx.BadRequest("invalid StorageClass name")
		}
		var p storageClassParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if p.UID == "" || p.ResourceVersion == "" {
			return httpx.BadRequest("uid and resource_version are required for StorageClass delete")
		}
		normalized, _ := json.Marshal(p)
		pl.Params = normalized
	case "pvc_create":
		var p pvcParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		p.Namespace = strings.TrimSpace(p.Namespace)
		if p.Namespace == "" {
			p.Namespace = pl.Namespace
		}
		if err := validatePVCIdentity(p.Namespace, p.Name); err != nil {
			return err
		}
		if !validAccessModes(p.AccessModes) {
			return httpx.BadRequest("invalid or empty access_modes")
		}
		if p.SnapshotName != "" {
			if err := validatePVCIdentity(p.Namespace, p.SnapshotName); err != nil {
				return httpx.BadRequest("invalid snapshot_name")
			}
			if p.VolumeName != "" {
				return httpx.BadRequest("volume_name cannot be specified when restoring from a snapshot")
			}
		}
		if _, err := parseStorageSize(p.Size); err != nil {
			return err
		}
		if p.VolumeMode != "" && p.VolumeMode != string(corev1.PersistentVolumeFilesystem) && p.VolumeMode != string(corev1.PersistentVolumeBlock) {
			return httpx.BadRequest("volume_mode must be Filesystem or Block")
		}
		pl.Namespace, pl.Name = p.Namespace, p.Name
		normalized, _ := json.Marshal(p)
		pl.Params = normalized
	case "pvc_update":
		if err := validatePVCIdentity(pl.Namespace, pl.Name); err != nil {
			return err
		}
		var p pvcParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if p.ResourceVersion == "" {
			return httpx.BadRequest("resource_version is required for PVC update")
		}
		if p.Size == "" {
			return httpx.BadRequest("size is required for PVC update")
		}
		if _, err := parseStorageSize(p.Size); err != nil {
			return err
		}
	case "pvc_delete":
		if err := validatePVCIdentity(pl.Namespace, pl.Name); err != nil {
			return err
		}
		var p pvcParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if p.UID == "" || p.ResourceVersion == "" || p.Confirm != pl.Namespace+"/"+pl.Name {
			return httpx.BadRequest("uid, resource_version and confirm=namespace/name are required")
		}
	case "pvc_batch_delete":
		var p pvcBatchParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if len(p.Items) == 0 || len(p.Items) > 100 {
			return httpx.BadRequest("items must contain 1..100 PVCs")
		}
		if p.Confirm != "DELETE" {
			return httpx.BadRequest("confirm=DELETE is required for batch deletion")
		}
		for _, item := range p.Items {
			if err := validatePVCIdentity(item.Namespace, item.Name); err != nil {
				return err
			}
			if item.UID == "" || item.ResourceVersion == "" {
				return httpx.BadRequest("each item needs uid and resource_version")
			}
		}
	case "pv_delete":
		if errs := validation.IsDNS1123Subdomain(pl.Name); len(errs) > 0 {
			return httpx.BadRequest("invalid PV name")
		}
		var p pvParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if p.UID == "" || p.ResourceVersion == "" || p.Confirm != pl.Name {
			return httpx.BadRequest("uid, resource_version and confirm=name are required")
		}
	case "pv_create", "pv_update":
		var p pvParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if p.Name == "" {
			p.Name = pl.Name
		}
		if errs := validation.IsDNS1123Subdomain(p.Name); len(errs) > 0 {
			return httpx.BadRequest("invalid PV name")
		}
		if _, err := parseStorageSize(p.Capacity); err != nil {
			return httpx.BadRequest("capacity: " + err.Error())
		}
		if !validAccessModes(p.AccessModes) {
			return httpx.BadRequest("invalid or empty access_modes")
		}
		if p.ReclaimPolicy == "" {
			p.ReclaimPolicy = string(corev1.PersistentVolumeReclaimRetain)
		}
		if p.ReclaimPolicy != string(corev1.PersistentVolumeReclaimDelete) && p.ReclaimPolicy != string(corev1.PersistentVolumeReclaimRetain) {
			return httpx.BadRequest("reclaim_policy must be Delete or Retain")
		}
		if p.VolumeMode == "" {
			p.VolumeMode = string(corev1.PersistentVolumeFilesystem)
		}
		if p.VolumeMode != string(corev1.PersistentVolumeFilesystem) && p.VolumeMode != string(corev1.PersistentVolumeBlock) {
			return httpx.BadRequest("volume_mode must be Filesystem or Block")
		}
		if p.HostPath == "" && p.NFS == nil {
			return httpx.BadRequest("host_path or nfs is required for a static PV")
		}
		if p.HostPath != "" && p.NFS != nil {
			return httpx.BadRequest("host_path and nfs are mutually exclusive")
		}
		if p.NFS != nil && (strings.TrimSpace(p.NFS.Server) == "" || strings.TrimSpace(p.NFS.Path) == "") {
			return httpx.BadRequest("nfs.server and nfs.path are required")
		}
		if pl.Action == "pv_update" {
			if p.ResourceVersion == "" || p.UID == "" {
				return httpx.BadRequest("uid and resource_version are required for PV update")
			}
			if p.Name != pl.Name {
				return httpx.BadRequest("PV update name must match the URL")
			}
		}
		pl.Name = p.Name
		normalized, _ := json.Marshal(p)
		pl.Params = normalized
	case "snapshot_create", "snapshot_delete":
		var p snapshotParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if pl.Action == "snapshot_delete" {
			if pl.Namespace == "" || pl.Name == "" {
				return httpx.BadRequest("snapshot delete requires namespace and name in the URL")
			}
			if (p.Namespace != "" && p.Namespace != pl.Namespace) || (p.Name != "" && p.Name != pl.Name) {
				return httpx.BadRequest("snapshot delete identity must match the URL")
			}
			p.Namespace, p.Name = pl.Namespace, pl.Name
		}
		if p.Namespace == "" {
			p.Namespace = pl.Namespace
		}
		if p.Name == "" {
			p.Name = pl.Name
		}
		if err := validatePVCIdentity(p.Namespace, p.Name); err != nil {
			return err
		}
		if pl.Action == "snapshot_create" {
			if err := validatePVCIdentity(p.Namespace, p.SourcePVC); err != nil {
				return httpx.BadRequest("invalid source_pvc")
			}
			p.UID, p.ResourceVersion = "", ""
		} else if p.UID == "" || p.ResourceVersion == "" {
			return httpx.BadRequest("uid and resource_version are required for snapshot delete")
		}
		pl.Namespace, pl.Name = p.Namespace, p.Name
		normalized, _ := json.Marshal(p)
		pl.Params = normalized
	default:
		return httpx.BadRequest("unknown storage action " + pl.Action)
	}
	return nil
}

// ---------------------------------------------------------------- execute

func executeStorage(ctx context.Context, kc *kubeClients, pl changePayload) (any, error) {
	cs := kc.cs
	switch pl.Action {
	case "sc_create", "sc_update":
		var p storageClassParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return nil, err
		}
		reclaim := corev1.PersistentVolumeReclaimDelete
		if p.ReclaimPolicy != "" {
			reclaim = corev1.PersistentVolumeReclaimPolicy(p.ReclaimPolicy)
		}
		binding := storagev1.VolumeBindingImmediate
		if p.VolumeBindingMode != "" {
			binding = storagev1.VolumeBindingMode(p.VolumeBindingMode)
		}
		allow := p.AllowVolumeExpansion != nil && *p.AllowVolumeExpansion
		obj := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: p.Name}, Provisioner: p.Provisioner,
			ReclaimPolicy: &reclaim, VolumeBindingMode: &binding, AllowVolumeExpansion: &allow,
			Parameters: p.Parameters, MountOptions: p.MountOptions}
		if p.IsDefault != nil {
			obj.Annotations = map[string]string{}
			if *p.IsDefault {
				obj.Annotations["storageclass.kubernetes.io/is-default-class"] = "true"
			}
		}
		if pl.Action == "sc_create" {
			created, err := cs.StorageV1().StorageClasses().Create(ctx, obj, metav1.CreateOptions{FieldManager: fieldManager})
			if err != nil {
				return nil, err
			}
			return storageClassRow(*created), nil
		}
		current, err := cs.StorageV1().StorageClasses().Get(ctx, pl.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if current.ResourceVersion != p.ResourceVersion {
			return nil, httpx.Conflict("StorageClass changed since it was loaded; refresh and retry")
		}
		currentRow := storageClassRow(*current)
		if p.Provisioner != currentRow.Provisioner ||
			p.ReclaimPolicy != currentRow.ReclaimPolicy ||
			p.VolumeBindingMode != currentRow.VolumeBindingMode ||
			!sameStringMap(p.Parameters, current.Parameters) {
			return nil, httpx.BadRequest("StorageClass provisioner, reclaim policy, binding mode and parameters are immutable; create a new StorageClass")
		}
		current.AllowVolumeExpansion = &allow
		current.MountOptions = p.MountOptions
		if p.IsDefault != nil {
			if current.Annotations == nil {
				current.Annotations = map[string]string{}
			}
			if *p.IsDefault {
				current.Annotations["storageclass.kubernetes.io/is-default-class"] = "true"
			} else {
				delete(current.Annotations, "storageclass.kubernetes.io/is-default-class")
				delete(current.Annotations, "storageclass.beta.kubernetes.io/is-default-class")
			}
		}
		updated, err := cs.StorageV1().StorageClasses().Update(ctx, current, metav1.UpdateOptions{FieldManager: fieldManager})
		if err != nil {
			return nil, err
		}
		return storageClassRow(*updated), nil
	case "sc_delete":
		var p storageClassParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return nil, err
		}
		current, err := cs.StorageV1().StorageClasses().Get(ctx, pl.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if string(current.UID) != string(p.UID) || current.ResourceVersion != p.ResourceVersion {
			return nil, httpx.Conflict("StorageClass changed since it was loaded; refresh and retry")
		}
		if !p.Force {
			pvs, err := cs.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, err
			}
			for _, pv := range pvs.Items {
				if pv.Spec.StorageClassName == pl.Name {
					return nil, httpx.Conflict("StorageClass is referenced by PersistentVolume " + pv.Name + "; set force=true after verifying no data loss")
				}
			}
			pvcs, err := cs.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, err
			}
			for _, pvc := range pvcs.Items {
				if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName == pl.Name {
					return nil, httpx.Conflict("StorageClass is referenced by PVC " + pvc.Namespace + "/" + pvc.Name + "; set force=true after verifying no data loss")
				}
			}
		}
		uid, rv := p.UID, p.ResourceVersion
		if err := cs.StorageV1().StorageClasses().Delete(ctx, pl.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
		}); err != nil {
			return nil, err
		}
		return gin.H{"name": pl.Name, "deleted": true}, nil
	case "pvc_create":
		var p pvcParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return nil, err
		}
		q, err := parseStorageSize(p.Size)
		if err != nil {
			return nil, err
		}
		mode := corev1.PersistentVolumeFilesystem
		if p.VolumeMode != "" {
			mode = corev1.PersistentVolumeMode(p.VolumeMode)
		}
		spec := corev1.PersistentVolumeClaimSpec{AccessModes: p.AccessModes, VolumeMode: &mode,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: q}}}
		if p.SnapshotName != "" {
			snapshot, err := kc.dyn.Resource(snapshotGVR).Namespace(p.Namespace).Get(ctx, p.SnapshotName, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			status, _ := snapshot.Object["status"].(map[string]any)
			ready, _ := status["readyToUse"].(bool)
			if !ready {
				return nil, httpx.Conflict("VolumeSnapshot is not readyToUse; PVC restore was not submitted")
			}
			group := snapshotGroup
			spec.DataSource = &corev1.TypedLocalObjectReference{
				APIGroup: &group, Kind: "VolumeSnapshot", Name: p.SnapshotName,
			}
		}
		if p.StorageClass != "" {
			spec.StorageClassName = &p.StorageClass
		}
		if p.VolumeName != "" {
			spec.VolumeName = p.VolumeName
		}
		created, err := cs.CoreV1().PersistentVolumeClaims(p.Namespace).Create(ctx, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: p.Labels}, Spec: spec,
		}, metav1.CreateOptions{FieldManager: fieldManager})
		if err != nil {
			return nil, err
		}
		return pvcRow(*created), nil
	case "pvc_update":
		var p pvcParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return nil, err
		}
		current, err := cs.CoreV1().PersistentVolumeClaims(pl.Namespace).Get(ctx, pl.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if current.ResourceVersion != p.ResourceVersion {
			return nil, httpx.Conflict("PVC changed since it was loaded; refresh and retry")
		}
		if len(p.AccessModes) > 0 && !validAccessModes(p.AccessModes) {
			return nil, httpx.BadRequest("access_modes cannot be changed to an invalid set")
		}
		q, err := parseStorageSize(p.Size)
		if err != nil {
			return nil, err
		}
		old := current.Spec.Resources.Requests[corev1.ResourceStorage]
		if q.Cmp(old) < 0 {
			return nil, httpx.BadRequest("PVC storage requests cannot be shrunk")
		}
		if current.Spec.Resources.Requests == nil {
			current.Spec.Resources.Requests = corev1.ResourceList{}
		}
		current.Spec.Resources.Requests[corev1.ResourceStorage] = q
		if p.Labels != nil {
			current.Labels = p.Labels
		}
		updated, err := cs.CoreV1().PersistentVolumeClaims(pl.Namespace).Update(ctx, current, metav1.UpdateOptions{FieldManager: fieldManager})
		if err != nil {
			return nil, err
		}
		return pvcRow(*updated), nil
	case "pvc_delete":
		var p pvcParams
		_ = decodeParams(pl.Params, &p)
		return deletePVC(ctx, kc, pl.Namespace, pl.Name, p.UID, p.ResourceVersion, p.AllowReclaimDel)
	case "pvc_batch_delete":
		var p pvcBatchParams
		_ = decodeParams(pl.Params, &p)
		results := make([]gin.H, 0, len(p.Items))
		failed := 0
		for _, item := range p.Items {
			res, err := deletePVC(ctx, kc, item.Namespace, item.Name, item.UID, item.ResourceVersion, item.AllowReclaimDel)
			if err != nil {
				failed++
				results = append(results, gin.H{"namespace": item.Namespace, "name": item.Name, "status": "failed", "error": err.Error()})
				continue
			}
			results = append(results, gin.H{"namespace": item.Namespace, "name": item.Name, "status": "deleted", "result": res})
		}
		return gin.H{"failed": failed, "total": len(results), "items": results}, nil
	case "pv_delete":
		var p pvParams
		_ = decodeParams(pl.Params, &p)
		pv, err := cs.CoreV1().PersistentVolumes().Get(ctx, pl.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if string(pv.UID) != string(p.UID) || pv.ResourceVersion != p.ResourceVersion {
			return nil, httpx.Conflict("PersistentVolume changed since it was loaded; refresh and retry")
		}
		if (pv.Status.Phase == corev1.VolumeBound || pv.Spec.ClaimRef != nil) && !p.Force {
			return nil, httpx.Conflict("PersistentVolume is bound to a claim; set force=true only after verifying the claim was removed and data loss is acceptable")
		}
		uid, rv := p.UID, p.ResourceVersion
		if err := cs.CoreV1().PersistentVolumes().Delete(ctx, pl.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
		}); err != nil {
			return nil, err
		}
		return gin.H{"name": pl.Name, "reclaim_policy": string(pv.Spec.PersistentVolumeReclaimPolicy), "deleted": true}, nil
	case "pv_create", "pv_update":
		var p pvParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return nil, err
		}
		q, err := parseStorageSize(p.Capacity)
		if err != nil {
			return nil, err
		}
		reclaim := corev1.PersistentVolumeReclaimPolicy(p.ReclaimPolicy)
		mode := corev1.PersistentVolumeMode(p.VolumeMode)
		obj := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: pl.Name, Labels: p.Labels},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:    corev1.ResourceList{corev1.ResourceStorage: q},
				AccessModes: p.AccessModes, PersistentVolumeReclaimPolicy: reclaim,
				StorageClassName: p.StorageClass, VolumeMode: &mode,
			},
		}
		if p.HostPath != "" {
			obj.Spec.PersistentVolumeSource.HostPath = &corev1.HostPathVolumeSource{Path: p.HostPath}
		}
		if p.NFS != nil {
			obj.Spec.PersistentVolumeSource.NFS = &corev1.NFSVolumeSource{Server: p.NFS.Server, Path: p.NFS.Path, ReadOnly: p.NFS.ReadOnly}
		}
		if pl.Action == "pv_create" {
			created, err := cs.CoreV1().PersistentVolumes().Create(ctx, obj, metav1.CreateOptions{FieldManager: fieldManager})
			if err != nil {
				return nil, err
			}
			return pvRow(*created), nil
		}
		current, err := cs.CoreV1().PersistentVolumes().Get(ctx, pl.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if string(current.UID) != string(p.UID) || current.ResourceVersion != p.ResourceVersion {
			return nil, httpx.Conflict("PersistentVolume changed since it was loaded; refresh and retry")
		}
		if p.HostPath != "" && (current.Spec.HostPath == nil || current.Spec.HostPath.Path != p.HostPath) ||
			p.NFS != nil && (current.Spec.NFS == nil || current.Spec.NFS.Server != p.NFS.Server || current.Spec.NFS.Path != p.NFS.Path) {
			return nil, httpx.BadRequest("PV source is immutable; create a new PV instead")
		}
		current.Spec.Capacity = obj.Spec.Capacity
		current.Spec.AccessModes = obj.Spec.AccessModes
		current.Spec.PersistentVolumeReclaimPolicy = obj.Spec.PersistentVolumeReclaimPolicy
		current.Spec.StorageClassName = obj.Spec.StorageClassName
		current.Spec.VolumeMode = obj.Spec.VolumeMode
		if p.Labels != nil {
			current.Labels = p.Labels
		}
		updated, err := cs.CoreV1().PersistentVolumes().Update(ctx, current, metav1.UpdateOptions{FieldManager: fieldManager})
		if err != nil {
			return nil, err
		}
		return pvRow(*updated), nil
	case "snapshot_create", "snapshot_delete":
		capability := snapshotCapability(ctx, kc)
		if !capability.Supported {
			return nil, httpx.Conflict("VolumeSnapshot is unavailable: " + capability.Reason)
		}
		var p snapshotParams
		_ = decodeParams(pl.Params, &p)
		ri := kc.dyn.Resource(snapshotGVR).Namespace(p.Namespace)
		if pl.Action == "snapshot_create" {
			if _, err := cs.CoreV1().PersistentVolumeClaims(p.Namespace).Get(ctx, p.SourcePVC, metav1.GetOptions{}); err != nil {
				return nil, err
			}
			obj := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": snapshotGroup + "/v1", "kind": "VolumeSnapshot",
				"metadata": map[string]any{"name": p.Name, "namespace": p.Namespace, "labels": p.Labels},
				"spec": map[string]any{
					"source": map[string]any{"persistentVolumeClaimName": p.SourcePVC},
				},
			}}
			if p.ClassName != "" {
				obj.Object["spec"].(map[string]any)["volumeSnapshotClassName"] = p.ClassName
			}
			created, err := ri.Create(ctx, obj, metav1.CreateOptions{FieldManager: fieldManager})
			if err != nil {
				return nil, err
			}
			return snapshotRow(*created), nil
		}
		uid, rv := p.UID, p.ResourceVersion
		if err := ri.Delete(ctx, p.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil {
			return nil, err
		}
		return gin.H{"namespace": p.Namespace, "name": p.Name, "deleted": true}, nil
	}
	return nil, httpx.BadRequest("unknown storage action " + pl.Action)
}

func deletePVC(ctx context.Context, kc *kubeClients, namespace, name string, uid types.UID, resourceVersion string, allowReclaimDelete bool) (any, error) {
	pvc, err := kc.cs.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if string(pvc.UID) != string(uid) || pvc.ResourceVersion != resourceVersion {
		return nil, httpx.Conflict("PVC changed since it was loaded; refresh and retry")
	}
	if pvc.Spec.VolumeName != "" && !allowReclaimDelete {
		pv, err := kc.cs.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
		if err == nil && pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimDelete {
			return nil, httpx.Conflict("PVC is bound to a PersistentVolume with reclaimPolicy Delete; deleting it deletes the volume. Set allow_reclaim_delete=true after confirming the data loss")
		}
	}
	if err := kc.cs.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion},
	}); err != nil {
		return nil, err
	}
	return gin.H{"namespace": namespace, "name": name, "deleted": true}, nil
}
