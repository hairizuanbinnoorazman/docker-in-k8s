package compose

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hairizuanbinnoorazman/docker-in-k8s/internal/api"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestLoadTranslatesSupportedProjectGolden(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "message.txt")
	if err := os.WriteFile(configPath, []byte("hello-config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	composePath := writeCompose(t, dir, `name: golden
services:
  web:
    image: busybox:1.36
    entrypoint: ["sh", "-c"]
    command: ["sleep 300"]
    environment:
      MESSAGE: hello
    working_dir: /tmp
    expose: ["8080"]
    volumes: ["data:/data"]
    configs:
      - source: message
        target: /etc/message
    secrets:
      - source: api_token
        target: /run/secrets/token
    healthcheck:
      test: ["CMD", "true"]
      interval: 5s
      timeout: 2s
      retries: 4
    deploy:
      resources:
        limits:
          cpus: "0.5"
          memory: 32M
volumes:
  data: {}
configs:
  message:
    file: ./message.txt
secrets:
  api_token:
    external: true
    name: existing-token
`)
	plan, err := Load(context.Background(), LoadOptions{Files: []string{composePath}}, "workloads")
	if err != nil {
		t.Fatal(err)
	}
	type summary struct {
		Name          string
		Container     string
		Image         string
		Command, Args []string
		Env           []corev1.EnvVar
		WorkingDir    string
		Mounts        []api.Mount
		Service       *corev1.Service
		Volumes       []VolumePlan
		Secrets       []string
	}
	raw, err := json.MarshalIndent(summary{plan.Name, plan.Services[0].ContainerName, plan.Services[0].Spec.Image, plan.Services[0].Spec.Command, plan.Services[0].Spec.Args, plan.Services[0].Spec.Env, plan.Services[0].Spec.WorkingDir, plan.Services[0].Spec.Mounts, plan.Services[0].Service, plan.Volumes, plan.Secrets}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	goldenPath := filepath.Join("testdata", "plan.golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, append(raw, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != strings.TrimSpace(string(want)) {
		t.Fatalf("translated plan differs from golden:\n%s", raw)
	}
}

func TestLoadRejectsUnsupportedFields(t *testing.T) {
	tests := map[string]string{
		"build":        "services:\n  app:\n    build: .\n",
		"privileged":   "services:\n  app:\n    image: busybox\n    privileged: true\n",
		"bind mount":   "services:\n  app:\n    image: busybox\n    volumes: [\"./data:/data\"]\n",
		"host network": "services:\n  app:\n    image: busybox\n    network_mode: host\n",
		"devices":      "services:\n  app:\n    image: busybox\n    devices: [\"/dev/null:/dev/null\"]\n",
		"logging":      "services:\n  app:\n    image: busybox\n    logging:\n      driver: json-file\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := writeCompose(t, t.TempDir(), content)
			_, err := Load(context.Background(), LoadOptions{Files: []string{path}}, "workloads")
			if err == nil || !strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("error = %v, want explicit unsupported field error", err)
			}
		})
	}
}

func TestApplyIsIdempotentAndDownRetainsVolumes(t *testing.T) {
	dir := t.TempDir()
	path := writeCompose(t, dir, `name: repeatable
services:
  app:
    image: busybox:1.36
    command: ["sleep", "300"]
    expose: ["8080"]
    volumes: ["data:/data"]
volumes:
  data: {}
`)
	plan, err := Load(context.Background(), LoadOptions{Files: []string{path}}, "workloads")
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient := newDynamicClient()
	coreClient := kubernetesfake.NewSimpleClientset()
	clients := Clients{dynamicClient, coreClient}
	if err := plan.Apply(context.Background(), clients, "workloads"); err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(context.Background(), clients, "workloads"); err != nil {
		t.Fatal(err)
	}
	containers, _ := dynamicClient.Resource(api.GVR).Namespace("workloads").List(context.Background(), metav1.ListOptions{})
	if len(containers.Items) != 1 {
		t.Fatalf("containers = %d, want 1", len(containers.Items))
	}
	services, _ := coreClient.CoreV1().Services("workloads").List(context.Background(), metav1.ListOptions{})
	if len(services.Items) != 1 {
		t.Fatalf("services = %d, want 1", len(services.Items))
	}
	claims, _ := coreClient.CoreV1().PersistentVolumeClaims("workloads").List(context.Background(), metav1.ListOptions{})
	if len(claims.Items) != 1 {
		t.Fatalf("claims = %d, want 1", len(claims.Items))
	}
	if err := Down(context.Background(), clients, "workloads", plan.Name, false); err != nil {
		t.Fatal(err)
	}
	if _, err := coreClient.CoreV1().PersistentVolumeClaims("workloads").Get(context.Background(), plan.Volumes[0].ClaimName, metav1.GetOptions{}); err != nil {
		t.Fatalf("PVC was not retained: %v", err)
	}
}

func TestApplyRetriesDockerContainerUpdateConflict(t *testing.T) {
	dir := t.TempDir()
	path := writeCompose(t, dir, `name: retryable
services:
  app:
    image: busybox:1.36
`)
	plan, err := Load(context.Background(), LoadOptions{Files: []string{path}}, "workloads")
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient := newDynamicClient()
	clients := Clients{dynamicClient, kubernetesfake.NewSimpleClientset()}
	if err := plan.Apply(context.Background(), clients, "workloads"); err != nil {
		t.Fatal(err)
	}

	conflicts := 0
	dynamicClient.PrependReactor("update", "dockercontainers", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 {
			conflicts++
			return true, nil, apierrors.NewConflict(api.GVR.GroupResource(), plan.Services[0].ContainerName, nil)
		}
		return false, nil, nil
	})

	if err := plan.Apply(context.Background(), clients, "workloads"); err != nil {
		t.Fatalf("apply did not retry conflict: %v", err)
	}
	if conflicts != 1 {
		t.Fatalf("conflicts = %d, want 1", conflicts)
	}
}

func TestPreflightFailureDoesNotMutate(t *testing.T) {
	dir := t.TempDir()
	path := writeCompose(t, dir, `name: atomic
services:
  app:
    image: busybox
    secrets: [missing]
secrets:
  missing:
    external: true
`)
	plan, err := Load(context.Background(), LoadOptions{Files: []string{path}}, "workloads")
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient := newDynamicClient()
	coreClient := kubernetesfake.NewSimpleClientset()
	clients := Clients{dynamicClient, coreClient}
	if err := plan.Apply(context.Background(), clients, "workloads"); err == nil {
		t.Fatal("expected missing secret failure")
	}
	containers, _ := dynamicClient.Resource(api.GVR).Namespace("workloads").List(context.Background(), metav1.ListOptions{})
	if len(containers.Items) != 0 {
		t.Fatal("preflight failure mutated workloads")
	}
	services, _ := coreClient.CoreV1().Services("workloads").List(context.Background(), metav1.ListOptions{})
	if len(services.Items) != 0 {
		t.Fatal("preflight failure mutated Services")
	}
}

func newDynamicClient() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{api.GVR: "DockerContainerList"})
}

func writeCompose(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigContentChangeUpdatesWorkloadHash(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "message.txt")
	if err := os.WriteFile(configPath, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	composePath := writeCompose(t, dir, `name: config-update
services:
  app:
    image: busybox
    configs:
      - source: message
        target: /etc/message
configs:
  message:
    file: ./message.txt
`)
	first, err := Load(context.Background(), LoadOptions{Files: []string{composePath}}, "workloads")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := Load(context.Background(), LoadOptions{Files: []string{composePath}}, "workloads")
	if err != nil {
		t.Fatal(err)
	}
	if api.SpecHash(first.Services[0].Spec) == api.SpecHash(second.Services[0].Spec) {
		t.Fatal("config content change did not change workload hash")
	}
}
