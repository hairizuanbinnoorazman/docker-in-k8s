package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
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
	ProjectLabel   = "dockube.io/compose-project"
	ServiceLabel   = "dockube.io/compose-service"
	ConfigHashAnno = "dockube.io/config-hash"
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
	Env          []corev1.EnvVar
	WorkingDir   string
	Ports        []corev1.ContainerPort
	Mounts       []Mount
	Resources    corev1.ResourceRequirements
	Readiness    *corev1.Probe
	Liveness     *corev1.Probe
	Startup      *corev1.Probe
	Labels       map[string]string
}

type Mount struct {
	Name        string
	Type        string
	Source      string
	Target      string
	ReadOnly    bool
	SubPath     string
	Items       map[string]string
	ContentHash string
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
	labels := map[string]any{ManagedByLabel: ManagedByValue}
	for key, value := range spec.Labels {
		labels[key] = value
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": Group + "/" + Version,
		"kind":       Kind,
		"metadata": map[string]any{
			"name": name, "namespace": namespace, "labels": labels,
		},
		"spec": specMap(spec),
	}}
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
	var extended struct {
		Env        []corev1.EnvVar             `json:"env"`
		WorkingDir string                      `json:"workingDir"`
		Ports      []corev1.ContainerPort      `json:"ports"`
		Mounts     []Mount                     `json:"mounts"`
		Resources  corev1.ResourceRequirements `json:"resources"`
		Readiness  *corev1.Probe               `json:"readiness"`
		Liveness   *corev1.Probe               `json:"liveness"`
		Startup    *corev1.Probe               `json:"startup"`
		Labels     map[string]string           `json:"labels"`
	}
	specObject, _, _ := unstructured.NestedMap(obj.Object, "spec")
	raw, err := json.Marshal(specObject)
	if err != nil {
		return ContainerSpec{}, err
	}
	if err := json.Unmarshal(raw, &extended); err != nil {
		return ContainerSpec{}, fmt.Errorf("decode extended container spec: %w", err)
	}
	if desiredState == "" {
		desiredState = "Running"
	}
	return ContainerSpec{
		Image: image, Command: command, Args: args, DesiredState: desiredState, Stdin: stdin, TTY: tty,
		Env: extended.Env, WorkingDir: extended.WorkingDir, Ports: extended.Ports, Mounts: extended.Mounts,
		Resources: extended.Resources, Readiness: extended.Readiness, Liveness: extended.Liveness,
		Startup: extended.Startup, Labels: extended.Labels,
	}, nil
}

func SetSpec(obj *unstructured.Unstructured, spec ContainerSpec) error {
	return unstructured.SetNestedMap(obj.Object, specMap(spec), "spec")
}

func Status(obj *unstructured.Unstructured) ContainerStatus {
	exitCode, hasExitCode, _ := unstructured.NestedInt64(obj.Object, "status", "exitCode")
	id := nestedString(obj, "status", "containerID")
	if id == "" && obj.GetUID() != "" {
		id = ContainerID(string(obj.GetUID()))
	}
	return ContainerStatus{
		ID: id, Phase: nestedString(obj, "status", "phase"), PodName: nestedString(obj, "status", "currentPodName"),
		Reason: nestedString(obj, "status", "reason"), CreatedAt: obj.GetCreationTimestamp().UTC().Format("2006-01-02T15:04:05Z"),
		StartedAt: nestedString(obj, "status", "startedAt"), FinishedAt: nestedString(obj, "status", "finishedAt"),
		ExitCode: exitCode, HasExitCode: hasExitCode, ResourceVersion: obj.GetResourceVersion(),
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
	labels := map[string]string{ManagedByLabel: ManagedByValue, NameLabel: obj.GetName()}
	for key, value := range spec.Labels {
		labels[key] = value
	}
	volumes := make([]corev1.Volume, 0, len(spec.Mounts))
	volumeMounts := make([]corev1.VolumeMount, 0, len(spec.Mounts))
	for _, mount := range spec.Mounts {
		volume := corev1.Volume{Name: mount.Name}
		switch mount.Type {
		case "pvc":
			volume.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{ClaimName: mount.Source, ReadOnly: mount.ReadOnly}
		case "configMap":
			volume.ConfigMap = &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: mount.Source}, Items: keyToPathItems(mount.Items)}
		case "secret":
			volume.Secret = &corev1.SecretVolumeSource{SecretName: mount.Source, Items: keyToPathItems(mount.Items)}
		default:
			continue
		}
		volumes = append(volumes, volume)
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: mount.Name, MountPath: mount.Target, ReadOnly: mount.ReadOnly, SubPath: mount.SubPath})
	}
	ownerController := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: PodName(obj.GetName(), string(obj.GetUID())), Namespace: obj.GetNamespace(), Labels: labels,
			Annotations:     map[string]string{ConfigHashAnno: SpecHash(spec)},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: Group + "/" + Version, Kind: Kind, Name: obj.GetName(), UID: obj.GetUID(), Controller: &ownerController}},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &falseValue, RestartPolicy: corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &terminationGracePeriod, Volumes: volumes,
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &trueValue, RunAsUser: &runAsUser, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers: []corev1.Container{{
				Name: "main", Image: spec.Image, Command: spec.Command, Args: spec.Args, Stdin: spec.Stdin, TTY: spec.TTY,
				Env: spec.Env, WorkingDir: spec.WorkingDir, Ports: spec.Ports, VolumeMounts: volumeMounts, Resources: spec.Resources,
				ReadinessProbe: spec.Readiness, LivenessProbe: spec.Liveness, StartupProbe: spec.Startup,
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &falseValue, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
			}},
		},
	}
}

func SpecHash(spec ContainerSpec) string {
	copy := spec
	copy.DesiredState = ""
	raw, _ := json.Marshal(copy)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

func StatusMap(obj *unstructured.Unstructured, pod *corev1.Pod) map[string]any {
	status := map[string]any{"containerID": ContainerID(string(obj.GetUID())), "phase": string(pod.Status.Phase), "currentPodName": pod.Name}
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

func specMap(spec ContainerSpec) map[string]any {
	raw, _ := json.Marshal(struct {
		Image        string                      `json:"image"`
		Command      []string                    `json:"command"`
		Args         []string                    `json:"args"`
		DesiredState string                      `json:"desiredState"`
		Stdin        bool                        `json:"stdin"`
		TTY          bool                        `json:"tty"`
		Env          []corev1.EnvVar             `json:"env,omitempty"`
		WorkingDir   string                      `json:"workingDir,omitempty"`
		Ports        []corev1.ContainerPort      `json:"ports,omitempty"`
		Mounts       []Mount                     `json:"mounts,omitempty"`
		Resources    corev1.ResourceRequirements `json:"resources,omitempty"`
		Readiness    *corev1.Probe               `json:"readiness,omitempty"`
		Liveness     *corev1.Probe               `json:"liveness,omitempty"`
		Startup      *corev1.Probe               `json:"startup,omitempty"`
		Labels       map[string]string           `json:"labels,omitempty"`
	}{spec.Image, spec.Command, spec.Args, spec.DesiredState, spec.Stdin, spec.TTY, spec.Env, spec.WorkingDir, spec.Ports, spec.Mounts, spec.Resources, spec.Readiness, spec.Liveness, spec.Startup, spec.Labels})
	result := map[string]any{}
	_ = json.Unmarshal(raw, &result)
	return result
}

func keyToPathItems(items map[string]string) []corev1.KeyToPath {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]corev1.KeyToPath, 0, len(keys))
	for _, key := range keys {
		result = append(result, corev1.KeyToPath{Key: key, Path: items[key]})
	}
	return result
}
