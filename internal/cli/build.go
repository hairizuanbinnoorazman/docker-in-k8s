package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
)

const maxBuildContext = 700 * 1024

func newBuildCommand(opts *options) *cobra.Command {
	var tag, dockerfile, buildNamespace string
	var keep bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "build [flags] PATH",
		Short: "Build and push an image with BuildKit on Kubernetes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if tag == "" {
				return fmt.Errorf("an image name is required with --tag")
			}
			if buildNamespace == "" {
				buildNamespace = opts.namespace
			}
			archive, err := archiveBuildContext(args[0], dockerfile)
			if err != nil {
				return err
			}
			if len(archive) > maxBuildContext {
				return fmt.Errorf("compressed build context is %d bytes; the Kubernetes ConfigMap transport limit is %d bytes", len(archive), maxBuildContext)
			}
			return runBuild(cmd.Context(), opts, buildNamespace, tag, filepath.ToSlash(dockerfile), archive, timeout, keep)
		},
	}
	cmd.Flags().StringVarP(&tag, "tag", "t", "", "image name to push (including registry)")
	cmd.Flags().StringVarP(&dockerfile, "file", "f", "Dockerfile", "Dockerfile path relative to the build context")
	cmd.Flags().StringVar(&buildNamespace, "build-namespace", "dockube-system", "namespace in which BuildKit jobs run")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "maximum build time")
	cmd.Flags().BoolVar(&keep, "keep-build", false, "retain the build Job and context ConfigMap")
	return cmd
}

func archiveBuildContext(root, dockerfile string) ([]byte, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("build context %q is not a directory", root)
	}
	dockerfile = filepath.Clean(dockerfile)
	if filepath.IsAbs(dockerfile) || dockerfile == ".." || strings.HasPrefix(dockerfile, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("Dockerfile must be inside the build context")
	}
	if info, err := os.Stat(filepath.Join(root, dockerfile)); err != nil || info.IsDir() {
		if err == nil {
			err = fmt.Errorf("is a directory")
		}
		return nil, fmt.Errorf("Dockerfile %q: %w", dockerfile, err)
	}

	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		if entry.IsDir() && (rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator))) {
			return filepath.SkipDir
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		link := ""
		if fileInfo.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(fileInfo, link)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !fileInfo.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	if err != nil {
		return nil, err
	}
	return compressed.Bytes(), nil
}

func runBuild(ctx context.Context, opts *options, namespace, image, dockerfile string, archive []byte, timeout time.Duration, keep bool) error {
	name := "dockube-build-" + strings.ToLower(rand.String(8))
	configMaps := opts.core.CoreV1().ConfigMaps(namespace)
	jobs := opts.core.BatchV1().Jobs(namespace)
	_, err := configMaps.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"app.kubernetes.io/managed-by": "dockube"}},
		BinaryData: map[string][]byte{"context.tar.gz": archive},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create build context: %w", err)
	}
	if !keep {
		defer configMaps.Delete(context.Background(), name, metav1.DeleteOptions{})
	}

	backoff := int32(0)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"app.kubernetes.io/managed-by": "dockube"}},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/managed-by": "dockube", "dockube.io/build": name}},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: boolptr(false),
					SecurityContext:              &corev1.PodSecurityContext{FSGroup: int64ptr(1000)},
					Containers: []corev1.Container{{
						Name:    "buildkit",
						Image:   "moby/buildkit:rootless",
						Command: []string{"/bin/sh", "-ec"},
						Args: []string{`tar -xzf /input/context.tar.gz -C /workspace
exec buildctl-daemonless.sh build \
  --frontend dockerfile.v0 \
  --local context=/workspace \
  --local dockerfile=/workspace \
  --opt filename="$DOCKERFILE" \
  --output "type=image,name=$IMAGE,push=true,registry.insecure=true"`},
						Env:          []corev1.EnvVar{{Name: "IMAGE", Value: image}, {Name: "DOCKERFILE", Value: dockerfile}, {Name: "BUILDKITD_FLAGS", Value: "--oci-worker-no-process-sandbox"}},
						VolumeMounts: []corev1.VolumeMount{{Name: "context", MountPath: "/input", ReadOnly: true}, {Name: "workspace", MountPath: "/workspace"}, {Name: "buildkit", MountPath: "/home/user/.local/share/buildkit"}},
						// RootlessKit requires unconfined syscall/AppArmor policy and its setuid
						// newuidmap helper. The main process remains UID 1000 and receives no
						// host mounts or Kubernetes service-account token.
						SecurityContext: &corev1.SecurityContext{RunAsUser: int64ptr(1000), RunAsGroup: int64ptr(1000), AllowPrivilegeEscalation: boolptr(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}, AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}},
					}},
					Volumes: []corev1.Volume{{Name: "context", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}}}, {Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}, {Name: "buildkit", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
				},
			},
		},
	}
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create BuildKit job: %w", err)
	}
	if !keep {
		defer func() {
			policy := metav1.DeletePropagationBackground
			_ = jobs.Delete(context.Background(), name, metav1.DeleteOptions{PropagationPolicy: &policy})
		}()
	}

	fmt.Fprintf(opts.out, "Building %s with job %s/%s\n", image, namespace, name)
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err = waitForBuild(waitCtx, opts, namespace, name)
	logs, logErr := buildLogs(ctx, opts, namespace, name)
	if logErr == nil && len(logs) > 0 {
		_, _ = opts.out.Write(logs)
		if logs[len(logs)-1] != '\n' {
			fmt.Fprintln(opts.out)
		}
	}
	if err != nil {
		return err
	}
	if logErr != nil {
		return logErr
	}
	fmt.Fprintf(opts.out, "Successfully pushed %s\n", image)
	return nil
}

func waitForBuild(ctx context.Context, opts *options, namespace, name string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		job, err := opts.core.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for _, condition := range job.Status.Conditions {
			if condition.Status != corev1.ConditionTrue {
				continue
			}
			switch condition.Type {
			case batchv1.JobComplete:
				return nil
			case batchv1.JobFailed:
				return fmt.Errorf("BuildKit job failed: %s", condition.Message)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for BuildKit job: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func buildLogs(ctx context.Context, opts *options, namespace, name string) ([]byte, error) {
	pods, err := opts.core.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + name})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("BuildKit job %s has no Pod", name)
	}
	stream, err := opts.core.CoreV1().Pods(namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{Container: "buildkit"}).Stream(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("BuildKit logs are no longer available")
		}
		return nil, err
	}
	defer stream.Close()
	return io.ReadAll(stream)
}

func int64ptr(value int64) *int64 { return &value }
func boolptr(value bool) *bool    { return &value }
