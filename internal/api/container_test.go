package api

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

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

func TestPodForTranslatesComposeFieldsAndHash(t *testing.T) {
	obj := NewContainer("project-web", "workloads", ContainerSpec{Image: "busybox"})
	obj.SetUID(types.UID("container-uid"))
	spec := ContainerSpec{
		Image: "busybox", Env: []corev1.EnvVar{{Name: "MESSAGE", Value: "hello"}}, WorkingDir: "/work",
		Ports:  []corev1.ContainerPort{{ContainerPort: 8080}},
		Mounts: []Mount{{Name: "data", Type: "pvc", Source: "project-data", Target: "/data"}},
		Labels: map[string]string{ProjectLabel: "project", ServiceLabel: "web"},
	}
	pod := PodFor(obj, spec)
	container := pod.Spec.Containers[0]
	if container.Env[0].Value != "hello" || container.WorkingDir != "/work" || container.Ports[0].ContainerPort != 8080 {
		t.Fatalf("Compose fields were not translated: %+v", container)
	}
	if pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "project-data" {
		t.Fatalf("PVC = %+v", pod.Spec.Volumes[0])
	}
	if pod.Labels[ProjectLabel] != "project" || pod.Annotations[ConfigHashAnno] != SpecHash(spec) {
		t.Fatalf("ownership/hash metadata = %+v %+v", pod.Labels, pod.Annotations)
	}
	changed := spec
	changed.Image = "busybox:1.36"
	if SpecHash(spec) == SpecHash(changed) {
		t.Fatal("config hash did not change")
	}
}

func TestSetSpecRoundTripExtendedFields(t *testing.T) {
	obj := NewContainer("web", "workloads", ContainerSpec{Image: "old"})
	want := ContainerSpec{Image: "new", DesiredState: "Running", Env: []corev1.EnvVar{{Name: "A", Value: "B"}}, Labels: map[string]string{ProjectLabel: "demo"}}
	if err := SetSpec(obj, want); err != nil {
		t.Fatal(err)
	}
	got, err := Spec(obj)
	if err != nil {
		t.Fatal(err)
	}
	if got.Image != want.Image || got.Env[0].Value != "B" || got.Labels[ProjectLabel] != "demo" {
		t.Fatalf("round trip = %+v", got)
	}
}
