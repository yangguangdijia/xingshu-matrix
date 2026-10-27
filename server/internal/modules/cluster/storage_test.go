package cluster

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestStorageClassDeleteRequiresIdentity(t *testing.T) {
	for _, params := range []string{
		`{"force":false}`,
		`{"uid":"sc-uid","force":false}`,
		`{"resource_version":"7","force":false}`,
	} {
		pl := changePayload{Action: "sc_delete", Name: "fast", Params: json.RawMessage(params)}
		if err := validateStorageChange(&pl); err == nil {
			t.Fatalf("expected stale identity payload to be rejected: %s", params)
		}
	}

	pl := changePayload{Action: "sc_delete", Name: "fast",
		Params: json.RawMessage(`{"uid":"sc-uid","resource_version":"7","force":false}`)}
	if err := validateStorageChange(&pl); err != nil {
		t.Fatalf("valid identity payload rejected: %v", err)
	}
}

func TestPersistentVolumeCreateAndUpdateUseClusterChangePath(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	kc := &kubeClients{cs: cs}

	created, err := executeOn(ctx, kc, changePayload{
		Action: "pv_create", Name: "data-volume",
		Params: json.RawMessage(`{"name":"data-volume","capacity":"1Gi","access_modes":["ReadWriteOnce"],"host_path":"/data"}`),
	})
	if err != nil {
		t.Fatalf("PV create was not routed through executeOn: %v", err)
	}
	if row := created.(PersistentVolumeRow); row.Name != "data-volume" || row.HostPath != "/data" {
		t.Fatalf("unexpected created PV: %+v", row)
	}

	initial := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "data-volume", UID: types.UID("pv-uid"), ResourceVersion: "1"},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{"storage": resource.MustParse("1Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/data"},
			},
		},
	}
	cs = fake.NewSimpleClientset(initial)
	kc = &kubeClients{cs: cs}
	updated, err := executeOn(ctx, kc, changePayload{
		Action: "pv_update", Name: "data-volume",
		Params: json.RawMessage(`{"name":"data-volume","uid":"pv-uid","resource_version":"1","capacity":"2Gi","access_modes":["ReadWriteOnce"],"host_path":"/data"}`),
	})
	if err != nil {
		t.Fatalf("PV update was not routed through executeOn: %v", err)
	}
	if row := updated.(PersistentVolumeRow); row.Capacity != "2Gi" {
		t.Fatalf("unexpected updated PV: %+v", row)
	}
}

func TestStorageClassDeleteRejectsChangedResource(t *testing.T) {
	ctx := context.Background()
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{
		Name: "fast", UID: types.UID("current-uid"), ResourceVersion: "8",
	}}
	cs := fake.NewSimpleClientset(sc)
	deleteCalls := 0
	cs.PrependReactor("delete", "storageclasses", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleteCalls++
		return false, nil, nil
	})

	_, err := executeStorage(ctx, &kubeClients{cs: cs}, changePayload{
		Action: "sc_delete", Name: "fast",
		Params: json.RawMessage(`{"uid":"stale-uid","resource_version":"7","force":true}`),
	})
	if err == nil {
		t.Fatal("expected changed StorageClass identity to conflict")
	}
	if deleteCalls != 0 {
		t.Fatalf("delete called for stale StorageClass identity: %d", deleteCalls)
	}
}
