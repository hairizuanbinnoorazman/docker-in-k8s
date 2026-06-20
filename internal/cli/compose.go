package cli

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/hairizuanbinnoorazman/docker-in-k8s/internal/api"
	composeproject "github.com/hairizuanbinnoorazman/docker-in-k8s/internal/compose"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type composeOptions struct {
	files       []string
	projectName string
	workingDir  string
}

func newComposeCommand(opts *options) *cobra.Command {
	composeOpts := &composeOptions{}
	cmd := &cobra.Command{Use: "compose", Short: "Manage a Compose project on Kubernetes"}
	cmd.PersistentFlags().StringSliceVarP(&composeOpts.files, "file", "f", nil, "Compose configuration files")
	cmd.PersistentFlags().StringVarP(&composeOpts.projectName, "project-name", "p", "", "Compose project name")
	cmd.PersistentFlags().StringVar(&composeOpts.workingDir, "project-directory", "", "Compose project working directory")
	cmd.AddCommand(newComposeConfigCommand(opts, composeOpts), newComposeUpCommand(opts, composeOpts), newComposePSCommand(opts, composeOpts), newComposeLogsCommand(opts, composeOpts), newComposeStateCommand(opts, composeOpts, "stop", "Stopped"), newComposeStateCommand(opts, composeOpts, "start", "Running"), newComposeRestartCommand(opts, composeOpts), newComposeDownCommand(opts, composeOpts))
	return cmd
}

func loadCompose(cmd *cobra.Command, opts *options, composeOpts *composeOptions) (*composeproject.Plan, error) {
	return composeproject.Load(cmd.Context(), composeproject.LoadOptions{Files: composeOpts.files, ProjectName: composeOpts.projectName, WorkingDir: composeOpts.workingDir}, opts.namespace)
}

func composeClients(opts *options) composeproject.Clients {
	return composeproject.Clients{Dynamic: opts.client, Core: opts.core}
}

func newComposeConfigCommand(opts *options, composeOpts *composeOptions) *cobra.Command {
	return &cobra.Command{Use: "config", Short: "Parse, normalize, and validate the Compose project", Args: cobra.NoArgs, Annotations: map[string]string{"dockube.local": "true"}, RunE: func(cmd *cobra.Command, _ []string) error {
		plan, err := loadCompose(cmd, opts, composeOpts)
		if err != nil {
			return err
		}
		_, err = opts.out.Write(plan.Normalized)
		return err
	}}
}

func newComposeUpCommand(opts *options, composeOpts *composeOptions) *cobra.Command {
	var detach bool
	cmd := &cobra.Command{Use: "up -d", Short: "Create or update a Compose project", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if !detach {
			return fmt.Errorf("dockube compose up currently requires --detach")
		}
		plan, err := loadCompose(cmd, opts, composeOpts)
		if err != nil {
			return err
		}
		if err := plan.Apply(cmd.Context(), composeClients(opts), opts.namespace); err != nil {
			return fmt.Errorf("Compose project %s apply failed: %w", plan.Name, err)
		}
		for _, service := range plan.Services {
			fmt.Fprintf(opts.out, "%s\tcreated or updated\n", service.Name)
		}
		return nil
	}}
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "run services in the background")
	return cmd
}

func newComposePSCommand(opts *options, composeOpts *composeOptions) *cobra.Command {
	return &cobra.Command{Use: "ps", Short: "List Compose services", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		plan, err := loadCompose(cmd, opts, composeOpts)
		if err != nil {
			return err
		}
		items, err := composeproject.List(cmd.Context(), composeClients(opts), opts.namespace, plan.Name)
		if err != nil {
			return err
		}
		writer := tabwriter.NewWriter(opts.out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "NAME\tIMAGE\tSTATUS\tSERVICE")
		for i := range items {
			spec, err := api.Spec(&items[i])
			if err != nil {
				return err
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", items[i].GetName(), spec.Image, displayStatus(api.Status(&items[i])), items[i].GetLabels()[api.ServiceLabel])
		}
		return writer.Flush()
	}}
}

func newComposeLogsCommand(opts *options, composeOpts *composeOptions) *cobra.Command {
	var follow bool
	var tail int64
	cmd := &cobra.Command{Use: "logs [SERVICE...]", Short: "Fetch Compose service logs", RunE: func(cmd *cobra.Command, args []string) error {
		plan, err := loadCompose(cmd, opts, composeOpts)
		if err != nil {
			return err
		}
		items, err := composeproject.List(cmd.Context(), composeClients(opts), opts.namespace, plan.Name)
		if err != nil {
			return err
		}
		selected := map[string]bool{}
		for _, name := range args {
			selected[name] = true
		}
		for i := range items {
			service := items[i].GetLabels()[api.ServiceLabel]
			if len(selected) > 0 && !selected[service] {
				continue
			}
			status := api.Status(&items[i])
			podName := status.PodName
			if podName == "" {
				podName = api.PodName(items[i].GetName(), string(items[i].GetUID()))
			}
			logOptions := &corev1.PodLogOptions{Container: "main", Follow: follow}
			if cmd.Flags().Changed("tail") {
				logOptions.TailLines = &tail
			}
			stream, err := opts.core.CoreV1().Pods(opts.namespace).GetLogs(podName, logOptions).Stream(cmd.Context())
			if err != nil {
				return fmt.Errorf("service %s logs: %w", service, err)
			}
			if len(items) > 1 {
				fmt.Fprintf(opts.out, "==> %s <==\n", service)
			}
			_, copyErr := io.Copy(opts.out, stream)
			closeErr := stream.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
		return nil
	}}
	cmd.Flags().BoolVar(&follow, "follow", false, "follow log output (one selected service recommended)")
	cmd.Flags().Int64VarP(&tail, "tail", "n", 0, "number of lines to show from the end")
	return cmd
}

func newComposeStateCommand(opts *options, composeOpts *composeOptions, name, state string) *cobra.Command {
	return &cobra.Command{Use: name, Short: name + " Compose services", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		plan, err := loadCompose(cmd, opts, composeOpts)
		if err != nil {
			return err
		}
		if err := composeproject.SetState(cmd.Context(), composeClients(opts), opts.namespace, plan.Name, state); err != nil {
			return err
		}
		for _, service := range plan.Services {
			if err := waitForPhase(cmd.Context(), opts, service.ContainerName, state, 60*time.Second); err != nil {
				return err
			}
			fmt.Fprintln(opts.out, service.Name)
		}
		return nil
	}}
}

func newComposeRestartCommand(opts *options, composeOpts *composeOptions) *cobra.Command {
	return &cobra.Command{Use: "restart", Short: "Restart Compose services", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		plan, err := loadCompose(cmd, opts, composeOpts)
		if err != nil {
			return err
		}
		clients := composeClients(opts)
		if err := composeproject.SetState(cmd.Context(), clients, opts.namespace, plan.Name, "Stopped"); err != nil {
			return err
		}
		for _, service := range plan.Services {
			if err := waitForPhase(cmd.Context(), opts, service.ContainerName, "Stopped", 60*time.Second); err != nil {
				return err
			}
		}
		if err := composeproject.SetState(cmd.Context(), clients, opts.namespace, plan.Name, "Running"); err != nil {
			return err
		}
		for _, service := range plan.Services {
			if err := waitForPhase(cmd.Context(), opts, service.ContainerName, "Running", 60*time.Second); err != nil {
				return err
			}
			fmt.Fprintln(opts.out, service.Name)
		}
		return nil
	}}
}

func newComposeDownCommand(opts *options, composeOpts *composeOptions) *cobra.Command {
	var volumes bool
	cmd := &cobra.Command{Use: "down", Short: "Remove Compose workloads and Services", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		plan, err := loadCompose(cmd, opts, composeOpts)
		if err != nil {
			return err
		}
		if err := composeproject.Down(cmd.Context(), composeClients(opts), opts.namespace, plan.Name, volumes); err != nil {
			return err
		}
		fmt.Fprintln(opts.out, plan.Name)
		return nil
	}}
	cmd.Flags().BoolVarP(&volumes, "volumes", "v", false, "also delete project persistent volume claims")
	return cmd
}

var _ = metav1.GetOptions{}
