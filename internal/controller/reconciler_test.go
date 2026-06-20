package controller

import (
	"context"
	"testing"

	"github.com/hairizuanbinnoorazman/docker-in-k8s/internal/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

func TestReconcileCreatesPod(t *testing.T) {
	obj := api.NewContainer("web", "workloads", api.ContainerSpec{
		Image:        "example.test/web:1",
		DesiredState: "Running",
	})
	obj.SetUID(types.UID("container-uid"))

	scheme := runtime.NewScheme()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme, obj)
	coreClient := kubernetesfake.NewSimpleClientset()
	reconciler := New(dynamicClient, coreClient, "workloads")

	if err := reconciler.Reconcile(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	updated, err := dynamicClient.Resource(api.GVR).Namespace("workloads").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	podName := api.PodName("web", "container-uid")
	pod, err := coreClient.CoreV1().Pods("workloads").Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.Containers[0].Image != "example.test/web:1" {
		t.Fatalf("image = %q", pod.Spec.Containers[0].Image)
	}
}

func TestReconcileStoppedDeletesPod(t *testing.T) {
	obj := api.NewContainer("web", "workloads", api.ContainerSpec{
		Image:        "example.test/web:1",
		DesiredState: "Stopped",
	})
	obj.SetUID(types.UID("container-uid"))
	pod := api.PodFor(obj, api.ContainerSpec{Image: "example.test/web:1"})

	scheme := runtime.NewScheme()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme, obj)
	coreClient := kubernetesfake.NewSimpleClientset(pod)
	reconciler := New(dynamicClient, coreClient, "workloads")

	if err := reconciler.Reconcile(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	updated, err := dynamicClient.Resource(api.GVR).Namespace("workloads").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	_, err = coreClient.CoreV1().Pods("workloads").Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err == nil {
		t.Fatal("pod was not deleted")
	}
}

func TestReconcileFinalizerDeletesPod(t *testing.T) {
	obj := api.NewContainer("web", "workloads", api.ContainerSpec{Image: "example.test/web:1"})
	obj.SetUID(types.UID("container-uid"))
	obj.SetFinalizers([]string{api.Finalizer})
	now := metav1.Now()
	obj.SetDeletionTimestamp(&now)
	pod := api.PodFor(obj, api.ContainerSpec{Image: "example.test/web:1"})

	scheme := runtime.NewScheme()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme, obj)
	coreClient := kubernetesfake.NewSimpleClientset(pod)
	reconciler := New(dynamicClient, coreClient, "workloads")

	if err := reconciler.Reconcile(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	if _, err := coreClient.CoreV1().Pods("workloads").Get(context.Background(), pod.Name, metav1.GetOptions{}); err == nil {
		t.Fatal("pod was not deleted during finalization")
	}
	updated, err := dynamicClient.Resource(api.GVR).Namespace("workloads").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(updated.GetFinalizers(), api.Finalizer) {
		t.Fatal("finalizer was removed before pod deletion was confirmed")
	}
	if err := reconciler.Reconcile(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
}
