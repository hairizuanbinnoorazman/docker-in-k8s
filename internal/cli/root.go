package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/hairizuanbinnoorazman/docker-in-k8s/internal/api"
	"github.com/hairizuanbinnoorazman/docker-in-k8s/internal/kube"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

type options struct {
	namespace string
	out       io.Writer
	errOut    io.Writer
	client    dynamic.Interface
	core      kubernetes.Interface
	config    *rest.Config
}

func NewRootCommand() *cobra.Command {
	opts := &options{out: os.Stdout, errOut: os.Stderr}
	cmd := &cobra.Command{
		Use:           "dockube",
		Short:         "Run Docker-like workloads on Kubernetes",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Annotations["dockube.local"] == "true" {
				return nil
			}
			if opts.namespace == "" {
				return fmt.Errorf("namespace is required; set --namespace or DOCKUBE_NAMESPACE")
			}
			config, err := kube.Config()
			if err != nil {
				return err
			}
			clients, err := kube.NewClients(config)
			if err != nil {
				return err
			}
			opts.client = clients.Dynamic
			opts.core = clients.Core
			opts.config = config
			return nil
		},
	}
	cmd.SetOut(opts.out)
	cmd.SetErr(opts.errOut)
	cmd.PersistentFlags().StringVar(&opts.namespace, "namespace", os.Getenv("DOCKUBE_NAMESPACE"), "Kubernetes workload namespace")
	cmd.AddCommand(
		newBuildCommand(opts),
		newRunCommand(opts),
		newPSCommand(opts),
		newRMCommand(opts),
		newLogsCommand(opts),
		newStateCommand(opts, "stop", "Stopped"),
		newStateCommand(opts, "start", "Running"),
		newRestartCommand(opts),
		newExecCommand(opts),
		newComposeCommand(opts),
	)
	return cmd
}

func newExecCommand(opts *options) *cobra.Command {
	var interactive bool
	var tty bool
	cmd := &cobra.Command{
		Use:   "exec [flags] CONTAINER COMMAND [ARG...]",
		Short: "Run a command in a running container",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveName(cmd.Context(), opts, args[0])
			if err != nil {
				return err
			}
			obj, err := opts.client.Resource(api.GVR).Namespace(opts.namespace).Get(cmd.Context(), name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			podName, err := runningPodName(obj)
			if err != nil {
				return err
			}
			request := opts.core.CoreV1().RESTClient().Post().
				Resource("pods").
				Name(podName).
				Namespace(opts.namespace).
				SubResource("exec").
				VersionedParams(&corev1.PodExecOptions{
					Container: "main",
					Command:   args[1:],
					Stdin:     interactive,
					Stdout:    true,
					Stderr:    !tty,
					TTY:       tty,
				}, clientscheme.ParameterCodec)
			executor, err := remotecommand.NewSPDYExecutor(opts.config, "POST", request.URL())
			if err != nil {
				return err
			}
			streamOptions := remotecommand.StreamOptions{
				Stdout: opts.out,
				Stderr: opts.errOut,
				Tty:    tty,
			}
			if interactive {
				streamOptions.Stdin = os.Stdin
			}
			return executor.StreamWithContext(cmd.Context(), streamOptions)
		},
	}
	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "keep stdin open")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "allocate a TTY")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func runningPodName(obj *unstructured.Unstructured) (string, error) {
	status := api.Status(obj)
	if status.Phase != "Running" {
		return "", fmt.Errorf("container %s is not running", obj.GetName())
	}
	if status.PodName == "" {
		return "", fmt.Errorf("container %s has no active Pod", obj.GetName())
	}
	return status.PodName, nil
}

func newLogsCommand(opts *options) *cobra.Command {
	var follow bool
	var tail int64
	cmd := &cobra.Command{
		Use:   "logs CONTAINER",
		Short: "Fetch container logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveName(cmd.Context(), opts, args[0])
			if err != nil {
				return err
			}
			obj, err := opts.client.Resource(api.GVR).Namespace(opts.namespace).Get(cmd.Context(), name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			status := api.Status(obj)
			podName := status.PodName
			if podName == "" {
				podName = api.PodName(name, string(obj.GetUID()))
			}
			logOptions := &corev1.PodLogOptions{Container: "main", Follow: follow}
			if cmd.Flags().Changed("tail") {
				logOptions.TailLines = &tail
			}
			stream, err := opts.core.CoreV1().Pods(opts.namespace).GetLogs(podName, logOptions).Stream(cmd.Context())
			if err != nil {
				return err
			}
			defer stream.Close()
			_, err = io.Copy(opts.out, stream)
			return err
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow log output")
	cmd.Flags().Int64VarP(&tail, "tail", "n", 0, "number of lines to show from the end")
	return cmd
}

func newStateCommand(opts *options, commandName, desiredState string) *cobra.Command {
	return &cobra.Command{
		Use:   commandName + " CONTAINER [CONTAINER...]",
		Short: commandName + " logical containers",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, value := range args {
				name, err := resolveName(cmd.Context(), opts, value)
				if err != nil {
					return err
				}
				if err := setDesiredState(cmd.Context(), opts.client, opts.namespace, name, desiredState); err != nil {
					return err
				}
				if err := waitForPhase(cmd.Context(), opts, name, desiredState, 60*time.Second); err != nil {
					return err
				}
				fmt.Fprintln(opts.out, name)
			}
			return nil
		},
	}
}

func newRestartCommand(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "restart CONTAINER [CONTAINER...]",
		Short: "Restart logical containers",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, value := range args {
				name, err := resolveName(cmd.Context(), opts, value)
				if err != nil {
					return err
				}
				if err := setDesiredState(cmd.Context(), opts.client, opts.namespace, name, "Stopped"); err != nil {
					return err
				}
				if err := waitForPhase(cmd.Context(), opts, name, "Stopped", 60*time.Second); err != nil {
					return err
				}
				if err := setDesiredState(cmd.Context(), opts.client, opts.namespace, name, "Running"); err != nil {
					return err
				}
				if err := waitForPhase(cmd.Context(), opts, name, "Running", 60*time.Second); err != nil {
					return err
				}
				fmt.Fprintln(opts.out, name)
			}
			return nil
		},
	}
}

func setDesiredState(ctx context.Context, client dynamic.Interface, namespace, name, desiredState string) error {
	resource := client.Resource(api.GVR).Namespace(namespace)
	for attempt := 0; attempt < 5; attempt++ {
		obj, err := resource.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := unstructured.SetNestedField(obj.Object, desiredState, "spec", "desiredState"); err != nil {
			return err
		}
		if _, err := resource.Update(ctx, obj, metav1.UpdateOptions{}); err == nil {
			return nil
		} else if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("could not update %s after conflicts", name)
}

func waitForPhase(ctx context.Context, opts *options, name, phase string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		obj, err := opts.client.Resource(api.GVR).Namespace(opts.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if api.Status(obj).Phase == phase {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %s to become %s: %w", name, phase, ctx.Err())
		case <-ticker.C:
		}
	}
}

func newRunCommand(opts *options) *cobra.Command {
	var name string
	var detach bool
	var stdin bool
	var tty bool
	cmd := &cobra.Command{
		Use:   "run [flags] IMAGE [COMMAND] [ARG...]",
		Short: "Create and run a logical container",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				name = generatedName(args[0])
			}
			spec := api.ContainerSpec{
				Image: args[0], DesiredState: "Running", Stdin: stdin, TTY: tty,
			}
			if len(args) > 1 {
				spec.Command = []string{args[1]}
				spec.Args = args[2:]
			}
			obj := api.NewContainer(name, opts.namespace, spec)
			created, err := opts.client.Resource(api.GVR).Namespace(opts.namespace).Create(cmd.Context(), obj, metav1.CreateOptions{})
			if err != nil {
				return err
			}
			id := api.ContainerID(string(created.GetUID()))
			if detach {
				fmt.Fprintln(opts.out, id)
				return nil
			}
			return waitForTerminal(cmd.Context(), opts, created.GetName(), id)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "assign a name")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "run in background")
	cmd.Flags().BoolVarP(&stdin, "interactive", "i", false, "keep stdin open")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "allocate a TTY")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func newPSCommand(opts *options) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List logical containers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			list, err := opts.client.Resource(api.GVR).Namespace(opts.namespace).List(cmd.Context(), metav1.ListOptions{
				LabelSelector: api.ManagedByLabel + "=" + api.ManagedByValue,
			})
			if err != nil {
				return err
			}
			writer := tabwriter.NewWriter(opts.out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(writer, "CONTAINER ID\tIMAGE\tCREATED\tSTATUS\tNAMES")
			now := time.Now()
			for i := range list.Items {
				obj := &list.Items[i]
				status := api.Status(obj)
				if !all && status.Phase != "Running" {
					continue
				}
				spec, err := api.Spec(obj)
				if err != nil {
					return err
				}
				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
					status.ID, spec.Image, humanDuration(now.Sub(obj.GetCreationTimestamp().Time)), displayStatus(status), obj.GetName())
			}
			return writer.Flush()
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "show all containers")
	return cmd
}

func newRMCommand(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "rm CONTAINER [CONTAINER...]",
		Short: "Remove logical containers",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, name := range args {
				resolved, err := resolveName(cmd.Context(), opts, name)
				if err != nil {
					return err
				}
				if err := opts.client.Resource(api.GVR).Namespace(opts.namespace).Delete(cmd.Context(), resolved, metav1.DeleteOptions{}); err != nil {
					return err
				}
				if err := waitForDeletion(cmd.Context(), opts, resolved, 30*time.Second); err != nil {
					return err
				}
				fmt.Fprintln(opts.out, resolved)
			}
			return nil
		},
	}
}

func waitForDeletion(ctx context.Context, opts *options, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := opts.client.Resource(api.GVR).Namespace(opts.namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out removing container %s: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForTerminal(ctx context.Context, opts *options, name, id string) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		obj, err := opts.client.Resource(api.GVR).Namespace(opts.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		status := api.Status(obj)
		switch status.Phase {
		case "Succeeded":
			fmt.Fprintln(opts.out, id)
			return nil
		case "Failed":
			if status.HasExitCode {
				return fmt.Errorf("container exited with code %d: %s", status.ExitCode, status.Reason)
			}
			return fmt.Errorf("container failed: %s", status.Reason)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func resolveName(ctx context.Context, opts *options, value string) (string, error) {
	_, err := opts.client.Resource(api.GVR).Namespace(opts.namespace).Get(ctx, value, metav1.GetOptions{})
	if err == nil {
		return value, nil
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}
	list, err := opts.client.Resource(api.GVR).Namespace(opts.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: api.ManagedByLabel + "=" + api.ManagedByValue,
	})
	if err != nil {
		return "", err
	}
	var match string
	for i := range list.Items {
		status := api.Status(&list.Items[i])
		if len(value) <= len(status.ID) && status.ID[:len(value)] == value {
			if match != "" {
				return "", fmt.Errorf("container ID prefix %q is ambiguous", value)
			}
			match = list.Items[i].GetName()
		}
	}
	if match == "" {
		return "", fmt.Errorf("no such container: %s", value)
	}
	return match, nil
}

func generatedName(image string) string {
	return fmt.Sprintf("dockube-%d", time.Now().UnixNano())
}

func humanDuration(duration time.Duration) string {
	if duration < time.Minute {
		return fmt.Sprintf("%d seconds ago", max(0, int(duration.Seconds())))
	}
	if duration < time.Hour {
		return fmt.Sprintf("%d minutes ago", int(duration.Minutes()))
	}
	return fmt.Sprintf("%d hours ago", int(duration.Hours()))
}

func displayStatus(status api.ContainerStatus) string {
	switch status.Phase {
	case "":
		return "Created"
	case "Running":
		return "Up"
	case "Succeeded":
		return "Exited (0)"
	case "Failed":
		if status.HasExitCode {
			return fmt.Sprintf("Exited (%d)", status.ExitCode)
		}
		return "Failed"
	default:
		return status.Phase
	}
}
