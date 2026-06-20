package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hairizuanbinnoorazman/docker-in-k8s/internal/controller"
	"github.com/hairizuanbinnoorazman/docker-in-k8s/internal/kube"
)

func main() {
	var namespace string
	var interval time.Duration
	flag.StringVar(&namespace, "namespace", os.Getenv("DOCKUBE_NAMESPACE"), "namespace to reconcile; empty means all namespaces")
	flag.DurationVar(&interval, "interval", 2*time.Second, "reconciliation interval")
	flag.Parse()

	config, err := kube.Config()
	if err != nil {
		exit(err)
	}
	clients, err := kube.NewClients(config)
	if err != nil {
		exit(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	reconciler := controller.New(clients.Dynamic, clients.Core, namespace)
	if err := reconciler.Run(ctx, interval); err != nil && ctx.Err() == nil {
		exit(err)
	}
}

func exit(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
