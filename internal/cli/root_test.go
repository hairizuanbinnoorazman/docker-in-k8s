package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/hairizuanbinnoorazman/docker-in-k8s/internal/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
