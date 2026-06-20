package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hairizuanbinnoorazman/docker-in-k8s/internal/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestDisplayStatus(t *testing.T) {
	tests := []struct {
		status api.ContainerStatus
		want   string
	}{
		{api.ContainerStatus{}, "Created"},
		{api.ContainerStatus{Phase: "Running"}, "Up"},
		{api.ContainerStatus{Phase: "Succeeded"}, "Exited (0)"},
		{api.ContainerStatus{Phase: "Failed", HasExitCode: true, ExitCode: 7}, "Exited (7)"},
	}
	for _, test := range tests {
		if got := displayStatus(test.status); got != test.want {
			t.Errorf("displayStatus(%+v) = %q, want %q", test.status, got, test.want)
		}
	}
}

func TestSetDesiredState(t *testing.T) {
	obj := api.NewContainer("test", "workloads", api.ContainerSpec{Image: "busybox", DesiredState: "Running"})
	obj.SetUID(types.UID("uid"))
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), obj)
	if err := setDesiredState(context.Background(), client, "workloads", "test", "Stopped"); err != nil {
		t.Fatal(err)
	}
	updated, err := client.Resource(api.GVR).Namespace("workloads").Get(context.Background(), "test", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := api.Spec(updated)
	if err != nil {
		t.Fatal(err)
	}
	if spec.DesiredState != "Stopped" {
		t.Fatalf("desired state = %q", spec.DesiredState)
	}
}

func TestRunningPodName(t *testing.T) {
	obj := api.NewContainer("test", "workloads", api.ContainerSpec{Image: "busybox"})
	obj.Object["status"] = map[string]any{"phase": "Running", "currentPodName": "test-pod"}
	name, err := runningPodName(obj)
	if err != nil {
		t.Fatal(err)
	}
	if name != "test-pod" {
		t.Fatalf("pod name = %q", name)
	}

	obj.Object["status"] = map[string]any{"phase": "Stopped"}
	if _, err := runningPodName(obj); err == nil {
		t.Fatal("expected stopped container to be rejected")
	}
}

func TestHumanDuration(t *testing.T) {
	if got := humanDuration(2 * time.Minute); got != "2 minutes ago" {
		t.Fatalf("humanDuration = %q", got)
	}
}

func TestNewContainerCarriesManagedLabel(t *testing.T) {
	obj := api.NewContainer("test", "workloads", api.ContainerSpec{Image: "busybox"})
	if got := obj.GetLabels()[api.ManagedByLabel]; got != api.ManagedByValue {
		t.Fatalf("managed label = %q", got)
	}
	if !strings.Contains(obj.GetAPIVersion(), api.Group) {
		t.Fatalf("apiVersion = %q", obj.GetAPIVersion())
	}
	if obj.GetCreationTimestamp() != (metav1.Time{}) {
		t.Fatal("client object unexpectedly has a creation timestamp")
	}
}
