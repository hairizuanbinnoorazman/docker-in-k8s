package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	Group          = "dockube.io"
	Version        = "v1alpha1"
	Kind           = "DockerContainer"
	Resource       = "dockercontainers"
	ManagedByLabel = "app.kubernetes.io/managed-by"
	NameLabel      = "dockube.io/container-name"
	ManagedByValue = "dockube"
	Finalizer      = "dockube.io/container-cleanup"
)

var GVR = schema.GroupVersionResource{Group: Group, Version: Version, Resource: Resource}

type ContainerSpec struct {
	Image        string
	Command      []string
	Args         []string
	DesiredState string
	Stdin        bool
	TTY          bool
}

type ContainerStatus struct {
	ID              string
	Phase           string
	PodName         string
	Reason          string
	CreatedAt       string
	StartedAt       string
	FinishedAt      string
	ExitCode        int64
	HasExitCode     bool
	ResourceVersion string
}

func NewContainer(name, namespace string, spec ContainerSpec) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": Group + "/" + Version,
		"kind":       Kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels": map[string]any{
				ManagedByLabel: ManagedByValue,
			},
		},
		"spec": map[string]any{
			"image":        spec.Image,
			"command":      stringSliceToAny(spec.Command),
			"args":         stringSliceToAny(spec.Args),
			"desiredState": spec.DesiredState,
			"stdin":        spec.Stdin,
			"tty":          spec.TTY,
		},
	}}
	return obj
}

func Spec(obj *unstructured.Unstructured) (ContainerSpec, error) {
	image, _, _ := unstructured.NestedString(obj.Object, "spec", "image")
	if image == "" {
		return ContainerSpec{}, fmt.Errorf("spec.image is required")
	}
	command, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "command")
	args, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "args")
	desiredState, _, _ := unstructured.NestedString(obj.Object, "spec", "desiredState")
	stdin, _, _ := unstructured.NestedBool(obj.Object, "spec", "stdin")
	tty, _, _ := unstructured.NestedBool(obj.Object, "spec", "tty")
	if desiredState == "" {
		desiredState = "Running"
	}
	return ContainerSpec{
		Image: image, Command: command, Args: args, DesiredState: desiredState, Stdin: stdin, TTY: tty,
	}, nil
}

func Status(obj *unstructured.Unstructured) ContainerStatus {
	exitCode, hasExitCode, _ := unstructured.NestedInt64(obj.Object, "status", "exitCode")
	id := nestedString(obj, "status", "containerID")
	if id == "" && obj.GetUID() != "" {
		id = ContainerID(string(obj.GetUID()))
	}
	return ContainerStatus{
		ID:              id,
		Phase:           nestedString(obj, "status", "phase"),
		PodName:         nestedString(obj, "status", "currentPodName"),
		Reason:          nestedString(obj, "status", "reason"),
		CreatedAt:       obj.GetCreationTimestamp().UTC().Format("2006-01-02T15:04:05Z"),
		StartedAt:       nestedString(obj, "status", "startedAt"),
		FinishedAt:      nestedString(obj, "status", "finishedAt"),
		ExitCode:        exitCode,
		HasExitCode:     hasExitCode,
		ResourceVersion: obj.GetResourceVersion(),
	}
}

func ContainerID(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return hex.EncodeToString(sum[:])[:12]
}

func PodName(containerName, uid string) string {
	id := ContainerID(uid)
	name := strings.Trim(containerName, "-")
	maxName := 63 - 1 - len(id)
	if len(name) > maxName {
		name = name[:maxName]
	}
	return name + "-" + id
}

func PodFor(obj *unstructured.Unstructured, spec ContainerSpec) *corev1.Pod {
	falseValue := false
	trueValue := true
	runAsUser := int64(65532)
	terminationGracePeriod := int64(10)
	ownerController := true
	podName := PodName(obj.GetName(), string(obj.GetUID()))

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: obj.GetNamespace(),
			Labels: map[string]string{
				ManagedByLabel: ManagedByValue,
				NameLabel:      obj.GetName(),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: Group + "/" + Version,
				Kind:       Kind,
				Name:       obj.GetName(),
				UID:        obj.GetUID(),
				Controller: &ownerController,
			}},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken:  &falseValue,
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &terminationGracePeriod,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &trueValue,
				RunAsUser:    &runAsUser,
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{{
				Name:    "main",
				Image:   spec.Image,
				Command: spec.Command,
				Args:    spec.Args,
				Stdin:   spec.Stdin,
				TTY:     spec.TTY,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &falseValue,
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
				},
			}},
		},
	}
}

func StatusMap(obj *unstructured.Unstructured, pod *corev1.Pod) map[string]any {
	status := map[string]any{
		"containerID":    ContainerID(string(obj.GetUID())),
		"phase":          string(pod.Status.Phase),
		"currentPodName": pod.Name,
	}
	if pod.Status.Reason != "" {
		status["reason"] = pod.Status.Reason
	}
	if pod.Status.StartTime != nil {
		status["startedAt"] = pod.Status.StartTime.UTC().Format("2006-01-02T15:04:05Z")
	}
	if len(pod.Status.ContainerStatuses) > 0 {
		state := pod.Status.ContainerStatuses[0].State
		if state.Waiting != nil && state.Waiting.Reason != "" {
			status["reason"] = state.Waiting.Reason
		}
		if state.Terminated != nil {
			status["exitCode"] = state.Terminated.ExitCode
			status["finishedAt"] = state.Terminated.FinishedAt.UTC().Format("2006-01-02T15:04:05Z")
			if state.Terminated.Reason != "" {
				status["reason"] = state.Terminated.Reason
			}
		}
	}
	return status
}

func nestedString(obj *unstructured.Unstructured, fields ...string) string {
	value, _, _ := unstructured.NestedString(obj.Object, fields...)
	return value
}

func stringSliceToAny(values []string) []any {
	result := make([]any, len(values))
	for i := range values {
		result[i] = values[i]
	}
	return result
}
