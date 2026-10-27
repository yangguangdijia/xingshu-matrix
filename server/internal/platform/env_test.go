package platform

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodeAllocatableResources(t *testing.T) {
	nodes := []corev1.Node{
		{Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("4"),
			corev1.ResourceMemory:           resource.MustParse("8Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
		}}},
		{Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("2"),
			corev1.ResourceMemory:           resource.MustParse("4Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("50Gi"),
		}}},
	}

	got := nodeAllocatableResources(nodes)
	if got.CPU != 6 || got.Memory != 12*gib || got.Disk != 150*gib {
		t.Fatalf("nodeAllocatableResources() = %+v, want CPU=6 Memory=12Gi Disk=150Gi", got)
	}
}

func TestRequestedResourcesExcludesFinishedPods(t *testing.T) {
	requests := corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("500m"),
		corev1.ResourceMemory:           resource.MustParse("1Gi"),
		corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
	}
	pods := []corev1.Pod{
		{Status: corev1.PodStatus{Phase: corev1.PodRunning}, Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: requests}}},
		}},
		{Status: corev1.PodStatus{Phase: corev1.PodPending}, Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: requests}}},
		}},
		{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}, Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: requests}}},
		}},
		{Status: corev1.PodStatus{Phase: corev1.PodFailed}, Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: requests}}},
		}},
	}

	got := requestedResources(pods)
	if got.CPU != 1 || got.Memory != 2*gib || got.Disk != 4*gib {
		t.Fatalf("requestedResources() = %+v, want CPU=1 Memory=2Gi Disk=4Gi", got)
	}
}

func TestStorageClassInfo(t *testing.T) {
	classes := []storagev1.StorageClass{
		{ObjectMeta: metav1.ObjectMeta{Name: "z-fast"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "a-default", Annotations: map[string]string{
			"storageclass.kubernetes.io/is-default-class": "true",
		}}},
	}

	names, defaultName := storageClassInfo(classes)
	if len(names) != 2 || names[0] != "a-default" || names[1] != "z-fast" {
		t.Fatalf("storage class names = %v, want sorted names", names)
	}
	if defaultName == nil || *defaultName != "a-default" {
		t.Fatalf("default storage class = %v, want a-default", defaultName)
	}

	classes = append(classes, storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{
		Name: "b-default",
		Annotations: map[string]string{
			"storageclass.kubernetes.io/is-default-class": "true",
		},
	}})
	_, defaultName = storageClassInfo(classes)
	if defaultName != nil {
		t.Fatalf("ambiguous default storage class = %q, want unknown", *defaultName)
	}
}
