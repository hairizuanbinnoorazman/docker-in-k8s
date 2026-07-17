package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestBuildKitImageIsPinned(t *testing.T) {
	if buildkitImage != "moby/buildkit:v0.30.0-rootless" {
		t.Fatalf("buildkitImage = %q", buildkitImage)
	}
}

func TestBuildResources(t *testing.T) {
	resources := buildResources()
	wantRequests := map[corev1.ResourceName]string{
		corev1.ResourceCPU:              "250m",
		corev1.ResourceMemory:           "256Mi",
		corev1.ResourceEphemeralStorage: "1Gi",
	}
	wantLimits := map[corev1.ResourceName]string{
		corev1.ResourceCPU:              "2",
		corev1.ResourceMemory:           "2Gi",
		corev1.ResourceEphemeralStorage: "10Gi",
	}
	for name, want := range wantRequests {
		if got := resources.Requests[name]; got.String() != want {
			t.Errorf("request %s = %s, want %s", name, got.String(), want)
		}
	}
	for name, want := range wantLimits {
		if got := resources.Limits[name]; got.String() != want {
			t.Errorf("limit %s = %s, want %s", name, got.String(), want)
		}
	}
}

func TestRunBuildGeneratesConstrainedJobAndCleanupPolicy(t *testing.T) {
	root := t.TempDir()
	writeBuildFile(t, root, "Dockerfile", "FROM scratch\n")
	buildContext, err := prepareBuildContext(root, "Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	frontend := buildFrontendOptions{
		buildArgs: []string{"VERSION=1"},
		outputs:   []buildOutput{{kind: "registry", spec: "type=image,name=registry.test/app,push=true"}},
	}
	registry := buildRegistryOptions{authSecret: "registry-auth", caSecret: "registry-ca", cachePVC: "cache-pvc"}
	for _, keep := range []bool{true, false} {
		client := fake.NewSimpleClientset()
		opts := &options{core: client, out: io.Discard, errOut: io.Discard}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := runBuild(ctx, opts, "build-ns", []string{"registry.test/app"}, buildContext, registry, frontend, buildResultOptions{progress: "plain"}, 0, keep); err == nil {
			t.Fatal("canceled build unexpectedly succeeded")
		}
		jobs, err := client.BatchV1().Jobs("build-ns").List(context.Background(), metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !keep {
			if len(jobs.Items) != 0 {
				t.Fatal("temporary Job was not cleaned up")
			}
			continue
		}
		if len(jobs.Items) != 1 {
			t.Fatalf("kept Jobs = %d", len(jobs.Items))
		}
		job := jobs.Items[0]
		container := job.Spec.Template.Spec.Containers[0]
		if container.Image != buildkitImage || container.Resources.Limits.Cpu().String() != "2" {
			t.Fatalf("container image/resources = %s/%#v", container.Image, container.Resources)
		}
		if container.SecurityContext == nil || container.SecurityContext.RunAsUser == nil || *container.SecurityContext.RunAsUser != 1000 || container.SecurityContext.AllowPrivilegeEscalation == nil {
			t.Fatalf("security context = %#v", container.SecurityContext)
		}
		arguments := strings.Join(container.Args, " ")
		if !strings.Contains(arguments, "build-arg:VERSION=1") || !strings.Contains(arguments, "type=image,name=registry.test/app,push=true") {
			t.Fatalf("Job arguments = %s", arguments)
		}
		if envValue(container.Env, "DOCKER_CONFIG") != "/run/dockube/docker" || envValue(container.Env, "BUILDKITD_FLAGS") == "" {
			t.Fatalf("Job environment = %#v", container.Env)
		}
		if len(job.Spec.Template.Spec.Volumes) != 4 || job.Spec.Template.Spec.Volumes[1].PersistentVolumeClaim == nil {
			t.Fatalf("Job volumes = %#v", job.Spec.Template.Spec.Volumes)
		}
	}
}

func TestBuildRegistryConfigurationUsesTLSByDefault(t *testing.T) {
	env, mounts, volumes := buildRegistryConfiguration([]string{"registry.example/app:test"}, "Dockerfile", buildRegistryOptions{})
	if envValue(env, "DOCKERFILE") != "Dockerfile" || envValue(env, "BUILDKITD_FLAGS") == "" {
		t.Fatalf("build environment = %#v", env)
	}
	if len(mounts) != 2 || len(volumes) != 2 {
		t.Fatalf("default mounts/volumes = %d/%d", len(mounts), len(volumes))
	}
}

func TestBuildRegistryConfigurationAllowsExplicitInsecureRegistry(t *testing.T) {
	outputs, err := normalizeBuildOutputs(nil, true, []string{"registry.test/app"}, true, false)
	if err != nil || outputs[0].spec != "type=image,name=registry.test/app,push=true,registry.insecure=true" {
		t.Fatalf("outputs = %#v, %v", outputs, err)
	}
}

func TestBuildRegistryConfigurationMountsCredentialsAndCA(t *testing.T) {
	env, mounts, volumes := buildRegistryConfiguration([]string{"registry.test/app"}, "Dockerfile", buildRegistryOptions{authSecret: "registry-auth", caSecret: "registry-ca"})
	if got := envValue(env, "DOCKER_CONFIG"); got != "/run/dockube/docker" {
		t.Fatalf("DOCKER_CONFIG = %q", got)
	}
	if len(mounts) != 4 || mounts[2].MountPath != "/run/dockube/docker" || !mounts[2].ReadOnly {
		t.Fatalf("registry auth mount = %#v", mounts)
	}
	if mounts[3].MountPath != "/etc/ssl/certs/dockube-registry-ca.crt" || mounts[3].SubPath != "ca.crt" || !mounts[3].ReadOnly {
		t.Fatalf("registry CA mount = %#v", mounts[3])
	}
	if volumes[2].Secret == nil || volumes[2].Secret.SecretName != "registry-auth" || volumes[2].Secret.Items[0].Key != corev1.DockerConfigJsonKey || volumes[2].Secret.Items[0].Path != "config.json" {
		t.Fatalf("registry auth volume = %#v", volumes[2])
	}
	if volumes[3].Secret == nil || volumes[3].Secret.SecretName != "registry-ca" || volumes[3].Secret.Items[0].Key != "ca.crt" {
		t.Fatalf("registry CA volume = %#v", volumes[3])
	}
	for _, variable := range env {
		if strings.Contains(variable.Value, "registry-auth") || strings.Contains(variable.Value, "registry-ca") {
			t.Fatalf("secret name leaked into environment: %#v", variable)
		}
	}
}

func TestBuildRegistryConfigurationSupportsMultipleTags(t *testing.T) {
	outputs, err := normalizeBuildOutputs(nil, true, []string{"registry.test/app:v1", "registry.test/app:latest"}, false, false)
	if err != nil || outputs[0].spec != `type=image,"name=registry.test/app:v1,registry.test/app:latest",push=true` {
		t.Fatalf("outputs = %#v, %v", outputs, err)
	}
}

func TestBuildFrontendArguments(t *testing.T) {
	got := buildFrontendArguments(buildFrontendOptions{
		buildArgs:     []string{"VERSION=1", "EMPTY="},
		target:        "release",
		labels:        []string{"org.example.version=1"},
		platforms:     []string{"linux/amd64", "linux/arm64"},
		noCache:       true,
		noCacheFilter: []string{"install", "test"},
		pull:          true,
		cacheFrom:     []string{"type=registry,ref=registry.test/cache"},
		cacheTo:       []string{"type=inline"},
		secrets:       []buildSecret{{id: "token", data: []byte("must-not-leak")}},
		ssh:           []buildSSH{{id: "default", data: []byte("private-key-must-not-leak")}},
		network:       "none",
		addHosts:      []string{"example.internal=203.0.113.10", "v6.internal=[2001:db8::10]"},
		shmSize:       32 * 1024 * 1024,
		namedContexts: []buildNamedContext{{name: "assets"}},
		outputs:       []buildOutput{{spec: "type=oci,dest=/workspace/.dockube-output-0.tar"}},
		attests:       []string{"attest:provenance=mode=max"},
		call:          "check",
	})
	want := []string{
		"--opt", "build-arg:VERSION=1",
		"--opt", "build-arg:EMPTY=",
		"--opt", "target=release",
		"--opt", "label:org.example.version=1",
		"--opt", "platform=linux/amd64,linux/arm64",
		"--no-cache",
		"--opt", "no-cache=install,test",
		"--opt", "image-resolve-mode=pull",
		"--opt", "force-network-mode=none",
		"--opt", "add-hosts=example.internal=203.0.113.10,v6.internal=[2001:db8::10]",
		"--opt", "shm-size=33554432",
		"--local", "assets=/home/user/.dockube-contexts/assets",
		"--opt", "context:assets=local:assets",
		"--output", "type=oci,dest=/workspace/.dockube-output-0.tar",
		"--opt", "attest:provenance=mode=max",
		"--opt", "call=check",
		"--import-cache", "type=registry,ref=registry.test/cache",
		"--export-cache", "type=inline",
		"--secret", "id=token,src=/run/dockube/secrets/token",
		"--ssh", "default=/run/dockube/secrets/ssh-default",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments = %#v, want %#v", got, want)
	}
	if strings.Contains(strings.Join(got, " "), "must-not-leak") {
		t.Fatal("secret or SSH key value leaked into BuildKit arguments")
	}
}

func TestNormalizeBuildNetwork(t *testing.T) {
	for _, test := range []struct {
		value   string
		want    string
		wantErr bool
	}{
		{value: "default"},
		{value: "none", want: "none"},
		{value: "host", wantErr: true},
		{value: "bridge", wantErr: true},
	} {
		got, err := normalizeBuildNetwork(test.value)
		if (err != nil) != test.wantErr || got != test.want {
			t.Errorf("normalizeBuildNetwork(%q) = %q, %v", test.value, got, err)
		}
	}
}

func TestNormalizeBuildHosts(t *testing.T) {
	got, err := normalizeBuildHosts([]string{"example.internal=203.0.113.10", "v6.internal:[2001:db8::10]"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"example.internal=203.0.113.10", "v6.internal=[2001:db8::10]"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hosts = %#v, want %#v", got, want)
	}
	for _, value := range []string{"missing-address", "bad host=127.0.0.1", "host=not-an-ip", "host=host-gateway"} {
		if _, err := normalizeBuildHosts([]string{value}); err == nil {
			t.Errorf("normalizeBuildHosts(%q) succeeded", value)
		}
	}
}

func TestNormalizeShmSize(t *testing.T) {
	got, err := normalizeShmSize("32m")
	if err != nil || got != 32*1024*1024 {
		t.Fatalf("normalizeShmSize(32m) = %d, %v", got, err)
	}
	for _, value := range []string{"0", "invalid"} {
		if _, err := normalizeShmSize(value); err == nil {
			t.Errorf("normalizeShmSize(%q) succeeded", value)
		}
	}
}

func TestBuildRegistryConfigurationAddsMemoryBackedSecretVolume(t *testing.T) {
	_, mounts, volumes := buildRegistryConfiguration([]string{"registry.test/app"}, "Dockerfile", buildRegistryOptions{secrets: true})
	if len(mounts) != 3 || mounts[2].MountPath != "/run/dockube/secrets" {
		t.Fatalf("secret mount = %#v", mounts)
	}
	if len(volumes) != 3 || volumes[2].EmptyDir == nil || volumes[2].EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Fatalf("secret volume = %#v", volumes)
	}
}

func TestBuildRegistryConfigurationUsesCachePVC(t *testing.T) {
	_, _, volumes := buildRegistryConfiguration([]string{"registry.test/app"}, "Dockerfile", buildRegistryOptions{cachePVC: "build-cache"})
	if volumes[1].PersistentVolumeClaim == nil || volumes[1].PersistentVolumeClaim.ClaimName != "build-cache" {
		t.Fatalf("buildkit cache volume = %#v", volumes[1])
	}
	if volumes[1].EmptyDir != nil {
		t.Fatal("PVC cache unexpectedly retained emptyDir")
	}
}

func TestNormalizeBuildArgsFromEnvironment(t *testing.T) {
	t.Setenv("VERSION", "from-env")
	got := normalizeBuildArgs([]string{"VERSION", "EXPLICIT=value", "MISSING"})
	want := []string{"VERSION=from-env", "EXPLICIT=value", "MISSING="}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("build args = %#v, want %#v", got, want)
	}
}

func TestNormalizeCommaSeparated(t *testing.T) {
	got := normalizeCommaSeparated([]string{"linux/amd64,linux/arm64", " linux/arm/v7 "})
	want := []string{"linux/amd64", "linux/arm64", "linux/arm/v7"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("values = %#v, want %#v", got, want)
	}
}

func TestNormalizeCacheEntries(t *testing.T) {
	got, err := normalizeCacheEntries([]string{"registry.test/cache", "type=registry,ref=other.test/cache,registry.insecure=false"}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"type=registry,ref=registry.test/cache,registry.insecure=true",
		"type=registry,ref=other.test/cache,registry.insecure=false",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cache entries = %#v, want %#v", got, want)
	}
	if _, err := normalizeCacheEntries([]string{"type=local,src=/cache"}, false, false); err == nil {
		t.Fatal("local cache import was accepted")
	}
	if got, err := normalizeCacheEntries([]string{"type=inline"}, false, true); err != nil || !reflect.DeepEqual(got, []string{"type=inline"}) {
		t.Fatalf("inline cache export = %#v, %v", got, err)
	}
}

func TestNormalizeBuildOutputs(t *testing.T) {
	outputs, err := normalizeBuildOutputs([]string{"type=oci,dest=result.oci", "type=local,dest=out"}, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 2 || outputs[0].spec != "type=oci,dest=/workspace/.dockube-output-0.tar" || outputs[1].podPath != "/workspace/.dockube-output-1" {
		t.Fatalf("outputs = %#v", outputs)
	}
	for _, values := range [][]string{nil, {"type=oci"}, {"type=unknown,dest=out"}, {"type=tar,dest=-"}} {
		if _, err := normalizeBuildOutputs(values, false, nil, false, false); err == nil {
			t.Errorf("outputs %#v were accepted", values)
		}
	}
}

func TestExtractLocalOutputRejectsTraversal(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o644})
	_ = tw.Close()
	_ = gz.Close()
	if err := extractLocalOutput(bytes.NewReader(archive.Bytes()), t.TempDir()); err == nil {
		t.Fatal("local output traversal was accepted")
	}
}

func TestBuildAnnotationsAttestsAndCalls(t *testing.T) {
	outputs := []buildOutput{{kind: "registry", spec: "type=image,name=registry.test/app,push=true"}}
	annotated, err := applyBuildAnnotations(outputs, []string{"org.example.version=1", "manifest[linux/amd64]:org.example.arch=amd64"})
	if err != nil || !strings.Contains(annotated[0].spec, "annotation.org.example.version=1") || !strings.Contains(annotated[0].spec, "annotation-manifest[linux/amd64].org.example.arch=amd64") {
		t.Fatalf("annotations = %#v, %v", annotated, err)
	}
	attests, err := normalizeBuildAttests(nil, "mode=max", "true")
	wantAttests := []string{"attest:provenance=mode=max", "attest:sbom="}
	if err != nil || !reflect.DeepEqual(attests, wantAttests) {
		t.Fatalf("attests = %#v, %v", attests, err)
	}
	if got, err := normalizeBuildCall("build", true); err != nil || got != "check" {
		t.Fatalf("call = %q, %v", got, err)
	}
	if _, err := normalizeBuildCall("invalid", false); err == nil {
		t.Fatal("invalid call was accepted")
	}
}

func TestBuildDebugMessageIsRedacted(t *testing.T) {
	message := buildDebugMessage("job", "namespace", buildFrontendOptions{
		buildArgs: []string{"TOKEN=must-not-leak"},
		secrets:   []buildSecret{{id: "secret-name", data: []byte("secret-value")}},
		ssh:       []buildSSH{{id: "ssh-name", data: []byte("private-key")}},
		outputs:   []buildOutput{{clientPath: "/sensitive/client/path"}},
	}, false, time.Minute)
	for _, sensitive := range []string{"must-not-leak", "secret-name", "secret-value", "ssh-name", "private-key", "/sensitive/client/path"} {
		if strings.Contains(message, sensitive) {
			t.Fatalf("debug message leaked %q: %s", sensitive, message)
		}
	}
	if !strings.Contains(message, "secrets=1") || !strings.Contains(message, "timeout-enabled=true") {
		t.Fatalf("debug message lacks safe diagnostics: %s", message)
	}
}

func TestParseBuildSecrets(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secretFile, []byte("file-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOKEN_ENV", "env-value")
	t.Setenv("AUTO_SECRET", "auto-value")
	secrets, err := parseBuildSecrets([]string{
		"type=file,id=file-secret,src=" + secretFile,
		"type=env,id=env-secret,env=TOKEN_ENV",
		"id=AUTO_SECRET",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []buildSecret{
		{id: "file-secret", data: []byte("file-value"), source: secretFile},
		{id: "env-secret", data: []byte("env-value")},
		{id: "AUTO_SECRET", data: []byte("auto-value")},
	}
	if !reflect.DeepEqual(secrets, want) {
		t.Fatalf("secrets = %#v, want %#v", secrets, want)
	}
}

func TestParseBuildSecretsRejectsInvalidInput(t *testing.T) {
	tests := []string{
		"type=env,id=../escape,env=MISSING",
		"type=unknown,id=token",
		"missing-equals",
	}
	for _, spec := range tests {
		if _, err := parseBuildSecrets([]string{spec}); err == nil {
			t.Errorf("expected %q to fail", spec)
		}
	}
	t.Setenv("DUPLICATE", "value")
	if _, err := parseBuildSecrets([]string{"id=DUPLICATE", "id=DUPLICATE"}); err == nil {
		t.Error("expected duplicate IDs to fail")
	}
}

func TestParseBuildSSH(t *testing.T) {
	privateKey := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(privateKey, []byte("private-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := parseBuildSSH([]string{"deploy=" + privateKey})
	if err != nil {
		t.Fatal(err)
	}
	want := []buildSSH{{id: "deploy", data: []byte("private-key"), source: privateKey}}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("SSH keys = %#v, want %#v", keys, want)
	}
}

func TestParseBuildSSHRejectsInvalidInput(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	if _, err := parseBuildSSH([]string{"default"}); err == nil {
		t.Fatal("expected missing agent source to fail")
	}
	if _, err := parseBuildSSH([]string{"../escape=/tmp/key"}); err == nil {
		t.Fatal("expected invalid ID to fail")
	}
}

func TestBuildPodStartupError(t *testing.T) {
	tests := []struct {
		name string
		pod  corev1.Pod
		want string
	}{
		{
			name: "unschedulable",
			pod: corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
				Reason: corev1.PodReasonUnschedulable, Message: "insufficient memory",
			}}}},
			want: "BuildKit Pod is unschedulable: insufficient memory",
		},
		{
			name: "image pull",
			pod: corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "not found"}},
			}}}},
			want: "BuildKit Pod startup failed (ImagePullBackOff): not found",
		},
		{name: "pending", pod: corev1.Pod{}, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := buildPodStartupError(&test.pod)
			if test.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestFinalBuildErrorSurfacesLogFailureWithoutMaskingBuildFailure(t *testing.T) {
	logErr := fmt.Errorf("pod logs are unavailable")
	if got := finalBuildError(nil, logErr); got != logErr {
		t.Fatalf("log error = %v", got)
	}
	buildErr := fmt.Errorf("Job failed")
	if got := finalBuildError(buildErr, fmt.Errorf("interrupted log stream")); got != buildErr {
		t.Fatalf("build error was masked: %v", got)
	}
}

func TestBuildWaitContext(t *testing.T) {
	withoutDeadline, cancel := buildWaitContext(t.Context(), 0)
	defer cancel()
	if _, ok := withoutDeadline.Deadline(); ok {
		t.Fatal("zero timeout unexpectedly created a deadline")
	}

	withDeadline, cancel := buildWaitContext(t.Context(), time.Minute)
	defer cancel()
	if _, ok := withDeadline.Deadline(); !ok {
		t.Fatal("positive timeout did not create a deadline")
	}
}

func TestNormalizeBuildResultOptions(t *testing.T) {
	tests := []struct {
		name     string
		progress string
		quiet    bool
		want     buildResultOptions
		wantErr  bool
	}{
		{name: "auto", progress: "auto", want: buildResultOptions{progress: "plain"}},
		{name: "plain", progress: "plain", want: buildResultOptions{progress: "plain"}},
		{name: "quiet flag", progress: "auto", quiet: true, want: buildResultOptions{progress: "quiet", quiet: true}},
		{name: "quiet progress", progress: "quiet", want: buildResultOptions{progress: "quiet", quiet: true}},
		{name: "raw JSON", progress: "rawjson", want: buildResultOptions{progress: "rawjson"}},
		{name: "conflict", progress: "rawjson", quiet: true, wantErr: true},
		{name: "unsupported", progress: "tty", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeBuildResultOptions(test.progress, test.quiet, "", "")
			if test.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("options = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestBuildCommandScriptResultHandshake(t *testing.T) {
	plain := buildCommandScript(false)
	if !strings.Contains(plain, "exec buildctl-daemonless.sh") || strings.Contains(plain, ".dockube-build-complete") {
		t.Fatalf("plain script = %q", plain)
	}
	result := buildCommandScript(true)
	for _, value := range []string{"status=$?", ".dockube-build-complete", ".dockube-result-collected", `exit "$status"`} {
		if !strings.Contains(result, value) {
			t.Fatalf("result script does not contain %q: %s", value, result)
		}
	}
}

func TestBuildImageID(t *testing.T) {
	metadata := []byte(`{"containerimage.config.digest":"sha256:abc123","containerimage.digest":"sha256:def456"}`)
	got, err := buildImageID(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if got != "sha256:abc123" {
		t.Fatalf("image ID = %q", got)
	}
	if _, err := buildImageID([]byte(`{"containerimage.digest":"sha256:def456"}`)); err == nil {
		t.Fatal("expected missing image ID to fail")
	}
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, variable := range env {
		if variable.Name == name {
			return variable.Value
		}
	}
	return ""
}

func TestArchiveBuildContext(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "message.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "ignored"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored", "secret.txt"), []byte("do not upload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".dockerignore"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	archive, err := archiveBuildContext(root, "Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		contents, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = string(contents)
	}
	if files["Dockerfile"] != "FROM scratch\n" || files["message.txt"] != "hello" {
		t.Fatalf("archive contents = %#v", files)
	}
	if _, exists := files["ignored/secret.txt"]; exists {
		t.Fatal(".dockerignore entry was included")
	}
	if files[".dockerignore"] != "ignored\n" {
		t.Fatal(".dockerignore was not included for the BuildKit frontend")
	}
}

func TestArchiveBuildContextDockerignoreNegation(t *testing.T) {
	root := t.TempDir()
	writeBuildFile(t, root, "Dockerfile", "FROM scratch\n")
	writeBuildFile(t, root, ".dockerignore", "output/**\n!output/keep.txt\n")
	writeBuildFile(t, root, "output/drop.txt", "drop")
	writeBuildFile(t, root, "output/keep.txt", "keep")

	files := archivedBuildFiles(t, root, "Dockerfile")
	if _, exists := files["output/drop.txt"]; exists {
		t.Fatal("ignored file was included")
	}
	if files["output/keep.txt"] != "keep" {
		t.Fatalf("negated file was not included: %#v", files)
	}
}

func TestArchiveBuildContextDockerignoreSyntax(t *testing.T) {
	root := t.TempDir()
	writeBuildFile(t, root, "Dockerfile", "FROM scratch\n")
	writeBuildFile(t, root, ".dockerignore", "\ufeff# comment\n/cache/\n**/*.tmp\n!important.tmp\n")
	writeBuildFile(t, root, "cache/data.txt", "cached")
	writeBuildFile(t, root, "nested/drop.tmp", "drop")
	writeBuildFile(t, root, "important.tmp", "keep")

	files := archivedBuildFiles(t, root, "Dockerfile")
	if _, exists := files["cache/data.txt"]; exists {
		t.Fatal("slash-delimited directory pattern was not applied")
	}
	if _, exists := files["nested/drop.tmp"]; exists {
		t.Fatal("wildcard pattern was not applied to a nested file")
	}
	if files["important.tmp"] != "keep" {
		t.Fatal("negated wildcard match was not included")
	}
}

func TestArchiveBuildContextDockerfileSpecificIgnoreTakesPrecedence(t *testing.T) {
	root := t.TempDir()
	writeBuildFile(t, root, "docker/build.Dockerfile", "FROM scratch\n")
	writeBuildFile(t, root, ".dockerignore", "root-only.txt\n")
	writeBuildFile(t, root, "docker/build.Dockerfile.dockerignore", "specific-only.txt\n")
	writeBuildFile(t, root, "root-only.txt", "root")
	writeBuildFile(t, root, "specific-only.txt", "specific")

	files := archivedBuildFiles(t, root, "docker/build.Dockerfile")
	if files["root-only.txt"] != "root" {
		t.Fatal("root .dockerignore incorrectly took precedence")
	}
	if _, exists := files["specific-only.txt"]; exists {
		t.Fatal("Dockerfile-specific ignore rule was not applied")
	}
	if _, exists := files["docker/build.Dockerfile.dockerignore"]; !exists {
		t.Fatal("selected ignore file was not included")
	}
}

func TestArchiveBuildContextAlwaysIncludesDockerfileAndIgnoreFile(t *testing.T) {
	root := t.TempDir()
	writeBuildFile(t, root, "Dockerfile", "FROM scratch\n")
	writeBuildFile(t, root, ".dockerignore", "Dockerfile\n.dockerignore\n")

	files := archivedBuildFiles(t, root, "Dockerfile")
	if files["Dockerfile"] != "FROM scratch\n" {
		t.Fatal("Dockerfile was excluded")
	}
	if files[".dockerignore"] == "" {
		t.Fatal(".dockerignore was excluded")
	}
}

func TestArchiveBuildContextIncludesGitWithoutIgnoreRule(t *testing.T) {
	root := t.TempDir()
	writeBuildFile(t, root, "Dockerfile", "FROM scratch\n")
	writeBuildFile(t, root, ".git/HEAD", "ref: refs/heads/main\n")

	files := archivedBuildFiles(t, root, "Dockerfile")
	if files[".git/HEAD"] == "" {
		t.Fatal("local .git directory should only be excluded by an ignore rule")
	}
}

func TestArchiveBuildContextRejectsDockerfileOutsideContext(t *testing.T) {
	if _, err := archiveBuildContext(t.TempDir(), "../Dockerfile"); err == nil {
		t.Fatal("expected path traversal to be rejected")
	}
}

func TestArchiveBuildContextRejectsInvalidIgnorePattern(t *testing.T) {
	root := t.TempDir()
	writeBuildFile(t, root, "Dockerfile", "FROM scratch\n")
	writeBuildFile(t, root, ".dockerignore", "[\n")
	if _, err := archiveBuildContext(root, "Dockerfile"); err == nil {
		t.Fatal("expected invalid ignore pattern to fail")
	}
}

func TestBuildContextFilesystemEdgeCases(t *testing.T) {
	root := t.TempDir()
	if _, err := prepareBuildContext(root, "Dockerfile"); err == nil {
		t.Fatal("missing Dockerfile was accepted")
	}
	if err := os.Mkdir(filepath.Join(root, "Dockerfile"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareBuildContext(root, "Dockerfile"); err == nil {
		t.Fatal("directory Dockerfile was accepted")
	}
	if err := os.Remove(filepath.Join(root, "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	writeBuildFile(t, root, "Dockerfile", "FROM scratch\n")
	writeBuildFile(t, root, "unreadable.txt", "private")
	if err := os.Chmod(filepath.Join(root, "unreadable.txt"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := archiveBuildContext(root, "Dockerfile"); err == nil || !strings.Contains(err.Error(), "not readable") {
		t.Fatalf("unreadable file error = %v", err)
	}
	_ = os.Chmod(filepath.Join(root, "unreadable.txt"), 0o600)
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := archiveBuildContext(root, "Dockerfile"); err == nil || !strings.Contains(err.Error(), "special file") {
		t.Fatalf("special file error = %v", err)
	}
}

func TestArchiveBuildContextPreservesSymlinkWithoutDereferencing(t *testing.T) {
	root := t.TempDir()
	writeBuildFile(t, root, "Dockerfile", "FROM scratch\n")
	external := filepath.Join(t.TempDir(), "outside-secret")
	if err := os.WriteFile(external, []byte("must-not-enter-archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	archive, err := archiveBuildContext(root, "Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	gz, _ := gzip.NewReader(bytes.NewReader(archive))
	tr := tar.NewReader(gz)
	found := false
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if header.Name == "link" {
			found = true
			if header.Typeflag != tar.TypeSymlink || header.Linkname != external {
				t.Fatalf("symlink header = %#v", header)
			}
		}
		contents, _ := io.ReadAll(tr)
		if strings.Contains(string(contents), "must-not-enter-archive") {
			t.Fatal("external symlink target entered archive")
		}
	}
	if !found {
		t.Fatal("symlink was not archived")
	}
}

func TestBuildContextExcludesCredentialSources(t *testing.T) {
	root := t.TempDir()
	writeBuildFile(t, root, "Dockerfile", "FROM scratch\n")
	secretPath := filepath.Join(root, "token.txt")
	sshPath := filepath.Join(root, "id_ed25519")
	writeBuildFile(t, root, "token.txt", "secret-value")
	writeBuildFile(t, root, "id_ed25519", "private-key")
	context, err := prepareBuildContext(root, "Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	excludeBuildCredentialSources(context,
		[]buildSecret{{source: secretPath}},
		[]buildSSH{{source: sshPath}},
	)
	var archive bytes.Buffer
	if err := writeBuildContext(context, &archive); err != nil {
		t.Fatal(err)
	}
	gz, _ := gzip.NewReader(bytes.NewReader(archive.Bytes()))
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if header.Name == "token.txt" || header.Name == "id_ed25519" {
			t.Fatalf("credential source %q entered context archive", header.Name)
		}
	}
}

func TestExtractBuildArchiveEnforcesSizeLimit(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "large.tar")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(file)
	if err := tw.WriteHeader(&tar.Header{Name: "too-large", Mode: 0o644, Size: maxArchiveContextSize + 1}); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := extractBuildArchive(archive); err == nil || !strings.Contains(err.Error(), "2 GiB") {
		t.Fatalf("size-limit error = %v", err)
	}
}

func TestPrepareBuildContextAcceptsTarArchive(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "context.tar")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(file)
	for name, contents := range map[string]string{"Dockerfile": "FROM scratch\n", "message.txt": "from tar"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(contents))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	context, err := prepareBuildContext(archive, "Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	defer context.cleanup()
	if contents, err := os.ReadFile(filepath.Join(context.root, "message.txt")); err != nil || string(contents) != "from tar" {
		t.Fatalf("extracted file = %q, %v", contents, err)
	}
}

func TestExtractBuildArchiveRejectsTraversalAndSpecialFiles(t *testing.T) {
	for _, test := range []struct {
		name     string
		typeflag byte
	}{
		{name: "../escape", typeflag: tar.TypeReg},
		{name: "device", typeflag: tar.TypeChar},
	} {
		archive := filepath.Join(t.TempDir(), "bad.tar")
		file, err := os.Create(archive)
		if err != nil {
			t.Fatal(err)
		}
		tw := tar.NewWriter(file)
		if err := tw.WriteHeader(&tar.Header{Name: test.name, Typeflag: test.typeflag, Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		_ = tw.Close()
		_ = file.Close()
		if _, err := extractBuildArchive(archive); err == nil {
			t.Errorf("archive entry %q was accepted", test.name)
		}
	}
}

func TestPrepareBuildContextAcceptsExplicitExternalDockerfile(t *testing.T) {
	root := t.TempDir()
	dockerfile := filepath.Join(t.TempDir(), "External.Dockerfile")
	writeBuildFile(t, root, "message.txt", "hello")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	context, err := prepareBuildContextWithFile(root, dockerfile, true)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := writeBuildContext(context, &archive); err != nil {
		t.Fatal(err)
	}
	if context.dockerfile != ".dockube.Dockerfile" {
		t.Fatalf("Dockerfile target = %q", context.dockerfile)
	}
	gz, _ := gzip.NewReader(bytes.NewReader(archive.Bytes()))
	tr := tar.NewReader(gz)
	found := false
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if header.Name == context.dockerfile {
			found = true
		}
	}
	if !found {
		t.Fatal("external Dockerfile was not injected")
	}
}

func TestPrepareBuildContextInput(t *testing.T) {
	var stdinArchive bytes.Buffer
	tw := tar.NewWriter(&stdinArchive)
	contents := "FROM scratch\n"
	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(contents))}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte(contents))
	_ = tw.Close()
	context, err := prepareBuildContextInput("-", "Dockerfile", false, bytes.NewReader(stdinArchive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if context.cleanup == nil {
		t.Fatal("stdin context did not register cleanup")
	}
	context.cleanup()

	root := t.TempDir()
	writeBuildFile(t, root, "message.txt", "hello")
	context, err = prepareBuildContextInput(root, "-", true, strings.NewReader("FROM scratch\nCOPY message.txt /message.txt\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer context.cleanup()
	if context.dockerfileSource == "" {
		t.Fatal("stdin Dockerfile was not configured as an injected source")
	}
	if _, err := prepareBuildContextInput("-", "-", true, strings.NewReader("")); err == nil {
		t.Fatal("ambiguous dual stdin input was accepted")
	}
}

func TestPrepareBuildContextRemoteTarAndText(t *testing.T) {
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	contents := "FROM scratch\n"
	_ = tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(contents))})
	_, _ = tw.Write([]byte(contents))
	_ = tw.Close()
	priorTransport := remoteHTTPTransport
	remoteHTTPTransport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := []byte("FROM scratch\n")
		if request.URL.Path == "/context.tar" {
			body = archive.Bytes()
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: request}, nil
	})
	defer func() { remoteHTTPTransport = priorTransport }()
	for _, path := range []string{"/context.tar", "/Dockerfile"} {
		context, err := prepareBuildContextInput("https://example.test"+path, "Dockerfile", false, strings.NewReader(""))
		if err != nil {
			t.Fatal(err)
		}
		context.cleanup()
	}
	if _, err := downloadRemoteInput("http://example.test/context.tar", 1024); err == nil {
		t.Fatal("unverified HTTP remote input was accepted")
	}
	if _, err := downloadRemoteInput("https://user:secret@example.test/context.tar", 1024); err == nil {
		t.Fatal("URL credentials were accepted")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestCloneGitContext(t *testing.T) {
	source := t.TempDir()
	writeBuildFile(t, source, "Dockerfile", "FROM scratch\n")
	commands := [][]string{{"init"}, {"config", "user.email", "test@example.invalid"}, {"config", "user.name", "Test"}, {"add", "Dockerfile"}, {"commit", "-m", "initial"}}
	for _, arguments := range commands {
		command := exec.Command("git", arguments...)
		command.Dir = source
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	cloned, err := cloneGitContext("file://" + source)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(cloned)
	if _, err := os.Stat(filepath.Join(cloned, "Dockerfile")); err != nil {
		t.Fatal(err)
	}
}

func TestParseNamedBuildContexts(t *testing.T) {
	root := t.TempDir()
	writeBuildFile(t, root, ".dockerignore", "ignored\n")
	writeBuildFile(t, root, "asset.txt", "asset")
	contexts, err := parseNamedBuildContexts([]string{"assets=" + root})
	if err != nil {
		t.Fatal(err)
	}
	defer contexts[0].context.cleanup()
	if len(contexts) != 1 || contexts[0].name != "assets" {
		t.Fatalf("named contexts = %#v", contexts)
	}
	for _, specs := range [][]string{{"invalid"}, {"bad/name=" + root}, {"assets=" + root, "assets=" + root}} {
		if _, err := parseNamedBuildContexts(specs); err == nil {
			t.Errorf("named contexts %#v were accepted", specs)
		}
	}
}

func writeBuildFile(t *testing.T, root, name, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func archivedBuildFiles(t *testing.T, root, dockerfile string) map[string]string {
	t.Helper()
	archive, err := archiveBuildContext(root, dockerfile)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	files := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		contents, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = string(contents)
	}
	return files
}
