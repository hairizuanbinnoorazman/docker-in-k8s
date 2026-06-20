package api

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestPodForUsesRestrictedDefaults(t *testing.T) {
	obj := NewContainer("web", "workloads", ContainerSpec{
		Image:        "example.test/web:1",
		DesiredState: "Running",
	})
	obj.SetUID(types.UID("container-uid"))

	pod := PodFor(obj, ContainerSpec{Image: "example.test/web:1"})

	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("service account token must not be mounted")
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Fatal("pod must run as non-root")
	}
	container := pod.Spec.Containers[0]
	if container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
		t.Fatal("privilege escalation must be disabled")
	}
	if len(container.SecurityContext.Capabilities.Drop) != 1 || container.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatal("all capabilities must be dropped")
	}
	if pod.Spec.TerminationGracePeriodSeconds == nil || *pod.Spec.TerminationGracePeriodSeconds != 10 {
		t.Fatal("termination grace period must default to Docker's 10 seconds")
	}
}

func TestContainerIDIsStableAndShort(t *testing.T) {
	first := ContainerID("abc")
	second := ContainerID("abc")
	if first != second {
		t.Fatalf("ID is not stable: %q != %q", first, second)
	}
	if len(first) != 12 {
		t.Fatalf("ID length = %d, want 12", len(first))
	}
}

func TestStatusDerivesIDBeforeControllerWritesStatus(t *testing.T) {
	obj := NewContainer("web", "workloads", ContainerSpec{Image: "example.test/web:1"})
	obj.SetUID(types.UID("container-uid"))
	if got := Status(obj).ID; got != ContainerID("container-uid") {
		t.Fatalf("status ID = %q", got)
	}
}
