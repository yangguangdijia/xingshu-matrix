package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

type HorizontalPodAutoscalerRow struct {
	Namespace       string                                      `json:"namespace"`
	Name            string                                      `json:"name"`
	UID             types.UID                                   `json:"uid"`
	ResourceVersion string                                      `json:"resource_version"`
	Labels          map[string]string                           `json:"labels"`
	Spec            autoscalingv2.HorizontalPodAutoscalerSpec   `json:"spec"`
	Status          autoscalingv2.HorizontalPodAutoscalerStatus `json:"status"`
	CreatedAt       metav1.Time                                 `json:"created_at"`
}

type hpaParams struct {
	Name            string                                     `json:"name"`
	Namespace       string                                     `json:"namespace"`
	UID             types.UID                                  `json:"uid"`
	ResourceVersion string                                     `json:"resource_version"`
	Labels          map[string]string                          `json:"labels"`
	Spec            *autoscalingv2.HorizontalPodAutoscalerSpec `json:"spec"`
}

type CronJobRow struct {
	Namespace       string                `json:"namespace"`
	Name            string                `json:"name"`
	UID             types.UID             `json:"uid"`
	ResourceVersion string                `json:"resource_version"`
	Labels          map[string]string     `json:"labels"`
	Spec            batchv1.CronJobSpec   `json:"spec"`
	Status          batchv1.CronJobStatus `json:"status"`
	CreatedAt       metav1.Time           `json:"created_at"`
}

type CronJobRunRow struct {
	Name           string       `json:"name"`
	UID            types.UID    `json:"uid"`
	Status         string       `json:"status"`
	Active         int32        `json:"active"`
	Succeeded      int32        `json:"succeeded"`
	Failed         int32        `json:"failed"`
	StartTime      *metav1.Time `json:"start_time,omitempty"`
	CompletionTime *metav1.Time `json:"completion_time,omitempty"`
	CreatedAt      metav1.Time  `json:"created_at"`
}

type cronJobParams struct {
	Name            string              `json:"name"`
	Namespace       string              `json:"namespace"`
	UID             types.UID            `json:"uid"`
	ResourceVersion string               `json:"resource_version"`
	Labels          map[string]string    `json:"labels"`
	Spec            *batchv1.CronJobSpec `json:"spec"`
}

type resourceIdentityParams struct {
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resource_version"`
}

func hpaRow(item autoscalingv2.HorizontalPodAutoscaler) HorizontalPodAutoscalerRow {
	return HorizontalPodAutoscalerRow{
		Namespace: item.Namespace, Name: item.Name, UID: item.UID, ResourceVersion: item.ResourceVersion,
		Labels: item.Labels, Spec: item.Spec, Status: item.Status, CreatedAt: item.CreationTimestamp,
	}
}

func cronJobRow(item batchv1.CronJob) CronJobRow {
	return CronJobRow{
		Namespace: item.Namespace, Name: item.Name, UID: item.UID, ResourceVersion: item.ResourceVersion,
		Labels: item.Labels, Spec: item.Spec, Status: item.Status, CreatedAt: item.CreationTimestamp,
	}
}

func cronJobRunRow(item batchv1.Job) CronJobRunRow {
	status := "Running"
	if item.Status.Failed > 0 {
		status = "Failed"
	}
	if item.Status.Succeeded > 0 {
		status = "Succeeded"
	}
	for _, condition := range item.Status.Conditions {
		if condition.Status == corev1.ConditionTrue && condition.Type == batchv1.JobFailed {
			status = "Failed"
		}
		if condition.Status == corev1.ConditionTrue && condition.Type == batchv1.JobComplete {
			status = "Succeeded"
		}
	}
	return CronJobRunRow{
		Name: item.Name, UID: item.UID, Status: status, Active: item.Status.Active,
		Succeeded: item.Status.Succeeded, Failed: item.Status.Failed,
		StartTime: item.Status.StartTime, CompletionTime: item.Status.CompletionTime,
		CreatedAt: item.CreationTimestamp,
	}
}

func (m *Module) listHPAs(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	list, err := kc.cs.AutoscalingV2().HorizontalPodAutoscalers(c.Query("namespace")).List(c.Request.Context(), metav1.ListOptions{})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	items := make([]HorizontalPodAutoscalerRow, 0, len(list.Items))
	for _, item := range list.Items {
		items = append(items, hpaRow(item))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace == items[j].Namespace {
			return items[i].Name < items[j].Name
		}
		return items[i].Namespace < items[j].Namespace
	})
	httpx.OK(c, gin.H{"items": items, "total": len(items)})
}

func (m *Module) listCronJobs(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	list, err := kc.cs.BatchV1().CronJobs(c.Query("namespace")).List(c.Request.Context(), metav1.ListOptions{})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	items := make([]CronJobRow, 0, len(list.Items))
	for _, item := range list.Items {
		items = append(items, cronJobRow(item))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace == items[j].Namespace {
			return items[i].Name < items[j].Name
		}
		return items[i].Namespace < items[j].Namespace
	})
	httpx.OK(c, gin.H{"items": items, "total": len(items)})
}

func (m *Module) listCronJobRuns(c *gin.Context) {
	_, kc, ok := m.withKube(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	cronJob, err := kc.cs.BatchV1().CronJobs(c.Param("ns")).Get(ctx, c.Param("name"), metav1.GetOptions{})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	list, err := kc.cs.BatchV1().Jobs(c.Param("ns")).List(ctx, metav1.ListOptions{
		LabelSelector: "batch.kubernetes.io/cronjob-name=" + cronJob.Name,
		Limit:         500,
	})
	if err != nil {
		httpx.Fail(c, kubeErr(err))
		return
	}
	items := make([]CronJobRunRow, 0, len(list.Items))
	for _, job := range list.Items {
		if !isOwnedByCronJob(&job, cronJob) {
			continue
		}
		items = append(items, cronJobRunRow(job))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt.Time) })
	httpx.OK(c, gin.H{"items": items, "total": len(items)})
}

func validateAutoscalingCronChange(pl *changePayload) error {
	switch pl.Action {
	case "hpa_create", "hpa_update":
		var p hpaParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if pl.Action == "hpa_create" {
			pl.Namespace, pl.Name = strings.TrimSpace(p.Namespace), strings.TrimSpace(p.Name)
		}
		if err := validatePVCIdentity(pl.Namespace, pl.Name); err != nil {
			return err
		}
		if pl.Action == "hpa_create" {
			if p.UID != "" || p.ResourceVersion != "" {
				return httpx.BadRequest("HPA create must not include uid or resource_version")
			}
		} else if p.UID == "" || p.ResourceVersion == "" {
			return httpx.BadRequest("uid and resource_version are required for HPA update")
		}
		if p.Spec == nil {
			return httpx.BadRequest("spec is required")
		}
		target := p.Spec.ScaleTargetRef
		if strings.TrimSpace(target.Name) == "" || strings.TrimSpace(target.Kind) == "" {
			return httpx.BadRequest("spec.scaleTargetRef kind and name are required")
		}
		if _, err := schema.ParseGroupVersion(target.APIVersion); err != nil {
			return httpx.BadRequest("spec.scaleTargetRef.apiVersion is invalid")
		}
		min := int32(1)
		if p.Spec.MinReplicas != nil {
			min = *p.Spec.MinReplicas
		}
		if min < 0 || p.Spec.MaxReplicas < 1 || p.Spec.MaxReplicas < min {
			return httpx.BadRequest("HPA replicas require 0 <= minReplicas <= maxReplicas and maxReplicas >= 1")
		}
		if len(p.Spec.Metrics) == 0 {
			return httpx.BadRequest("at least one autoscaling/v2 metric is required")
		}
		if pl.Action == "hpa_create" {
			p.UID, p.ResourceVersion = "", ""
		}
		normalized, _ := json.Marshal(p)
		pl.Params = normalized
	case "hpa_delete", "cronjob_delete", "cronjob_run":
		if err := validatePVCIdentity(pl.Namespace, pl.Name); err != nil {
			return err
		}
		var p resourceIdentityParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if p.UID == "" || p.ResourceVersion == "" {
			return httpx.BadRequest("uid and resource_version are required")
		}
		normalized, _ := json.Marshal(p)
		pl.Params = normalized
	case "cronjob_create", "cronjob_update":
		var p cronJobParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return err
		}
		if pl.Action == "cronjob_create" {
			pl.Namespace, pl.Name = strings.TrimSpace(p.Namespace), strings.TrimSpace(p.Name)
		}
		if err := validatePVCIdentity(pl.Namespace, pl.Name); err != nil {
			return err
		}
		if pl.Action == "cronjob_create" {
			if p.UID != "" || p.ResourceVersion != "" {
				return httpx.BadRequest("CronJob create must not include uid or resource_version")
			}
		} else if p.UID == "" || p.ResourceVersion == "" {
			return httpx.BadRequest("uid and resource_version are required for CronJob update")
		}
		if p.Spec == nil {
			return httpx.BadRequest("spec is required")
		}
		if strings.TrimSpace(p.Spec.Schedule) == "" {
			return httpx.BadRequest("spec.schedule is required")
		}
		if len(p.Spec.JobTemplate.Spec.Template.Spec.Containers) == 0 {
			return httpx.BadRequest("CronJob job template must include at least one container")
		}
		if p.Spec.JobTemplate.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever &&
			p.Spec.JobTemplate.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyOnFailure {
			return httpx.BadRequest("CronJob pod restartPolicy must be Never or OnFailure")
		}
		if pl.Action == "cronjob_create" {
			p.UID, p.ResourceVersion = "", ""
		}
		normalized, _ := json.Marshal(p)
		pl.Params = normalized
	default:
		return httpx.BadRequest("unknown autoscaling or CronJob action " + pl.Action)
	}
	return nil
}

func executeAutoscalingCron(ctx context.Context, kc *kubeClients, pl changePayload) (any, error) {
	switch pl.Action {
	case "hpa_create", "hpa_update":
		var p hpaParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return nil, err
		}
		client := kc.cs.AutoscalingV2().HorizontalPodAutoscalers(pl.Namespace)
		if pl.Action == "hpa_create" {
			created, err := client.Create(ctx, &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: pl.Name, Namespace: pl.Namespace, Labels: p.Labels},
				Spec:       *p.Spec,
			}, metav1.CreateOptions{FieldManager: fieldManager})
			if err != nil {
				return nil, err
			}
			return hpaRow(*created), nil
		}
		current, err := client.Get(ctx, pl.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if current.UID != p.UID || current.ResourceVersion != p.ResourceVersion {
			return nil, httpx.Conflict("HPA changed since it was loaded; refresh and retry")
		}
		current.Labels = p.Labels
		current.Spec = *p.Spec
		updated, err := client.Update(ctx, current, metav1.UpdateOptions{FieldManager: fieldManager})
		if err != nil {
			return nil, err
		}
		return hpaRow(*updated), nil
	case "hpa_delete":
		var p resourceIdentityParams
		_ = decodeParams(pl.Params, &p)
		uid, rv := p.UID, p.ResourceVersion
		if err := kc.cs.AutoscalingV2().HorizontalPodAutoscalers(pl.Namespace).Delete(ctx, pl.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
		}); err != nil {
			return nil, err
		}
		return gin.H{"namespace": pl.Namespace, "name": pl.Name, "deleted": true}, nil
	case "cronjob_create", "cronjob_update":
		var p cronJobParams
		if err := decodeParams(pl.Params, &p); err != nil {
			return nil, err
		}
		client := kc.cs.BatchV1().CronJobs(pl.Namespace)
		if pl.Action == "cronjob_create" {
			created, err := client.Create(ctx, &batchv1.CronJob{
				ObjectMeta: metav1.ObjectMeta{Name: pl.Name, Namespace: pl.Namespace, Labels: p.Labels},
				Spec:       *p.Spec,
			}, metav1.CreateOptions{FieldManager: fieldManager})
			if err != nil {
				return nil, err
			}
			return cronJobRow(*created), nil
		}
		current, err := client.Get(ctx, pl.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if current.UID != p.UID || current.ResourceVersion != p.ResourceVersion {
			return nil, httpx.Conflict("CronJob changed since it was loaded; refresh and retry")
		}
		current.Labels = p.Labels
		current.Spec = *p.Spec
		updated, err := client.Update(ctx, current, metav1.UpdateOptions{FieldManager: fieldManager})
		if err != nil {
			return nil, err
		}
		return cronJobRow(*updated), nil
	case "cronjob_delete":
		var p resourceIdentityParams
		_ = decodeParams(pl.Params, &p)
		uid, rv := p.UID, p.ResourceVersion
		if err := kc.cs.BatchV1().CronJobs(pl.Namespace).Delete(ctx, pl.Name, metav1.DeleteOptions{
			Preconditions:     &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
			PropagationPolicy: ptrPropagation(metav1.DeletePropagationForeground),
		}); err != nil {
			return nil, err
		}
		return gin.H{"namespace": pl.Namespace, "name": pl.Name, "deleted": true}, nil
	case "cronjob_run":
		var p resourceIdentityParams
		_ = decodeParams(pl.Params, &p)
		client := kc.cs.BatchV1().CronJobs(pl.Namespace)
		cronJob, err := client.Get(ctx, pl.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if cronJob.UID != p.UID || cronJob.ResourceVersion != p.ResourceVersion {
			return nil, httpx.Conflict("CronJob changed since it was loaded; refresh and retry")
		}
		suffix := "-manual-" + fmt.Sprint(time.Now().UTC().UnixNano())
		maxBase := 253 - len(suffix)
		base := cronJob.Name
		if len(base) > maxBase {
			base = strings.TrimRight(base[:maxBase], "-.")
		}
		name := base + suffix
		labels := make(map[string]string, len(cronJob.Spec.JobTemplate.Labels)+1)
		for key, value := range cronJob.Spec.JobTemplate.Labels {
			labels[key] = value
		}
		if labels["batch.kubernetes.io/cronjob-name"] == "" {
			labels["batch.kubernetes.io/cronjob-name"] = cronJob.Name
		}
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: cronJob.Namespace, Labels: labels,
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cronJob, schema.GroupVersionKind{
					Group: "batch", Version: "v1", Kind: "CronJob",
				})},
			},
			Spec: *cronJob.Spec.JobTemplate.Spec.DeepCopy(),
		}
		created, err := kc.cs.BatchV1().Jobs(pl.Namespace).Create(ctx, job, metav1.CreateOptions{FieldManager: fieldManager})
		if err != nil {
			return nil, err
		}
		return cronJobRunRow(*created), nil
	}
	return nil, httpx.BadRequest("unknown autoscaling or CronJob action " + pl.Action)
}

func ptrPropagation(policy metav1.DeletionPropagation) *metav1.DeletionPropagation {
	return &policy
}

func isOwnedByCronJob(job *batchv1.Job, cronJob *batchv1.CronJob) bool {
	for _, owner := range job.OwnerReferences {
		if owner.UID == cronJob.UID && owner.Kind == "CronJob" && owner.APIVersion == "batch/v1" {
			return true
		}
	}
	return job.Labels["batch.kubernetes.io/cronjob-name"] == cronJob.Name
}
