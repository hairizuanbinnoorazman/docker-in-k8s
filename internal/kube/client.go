package kube

import (
	"os"
	"path/filepath"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type Clients struct {
	Dynamic dynamic.Interface
	Core    kubernetes.Interface
}

func Config() (*rest.Config, error) {
	if config, err := rest.InClusterConfig(); err == nil {
		return config, nil
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path := os.Getenv("KUBECONFIG"); path != "" {
		loadingRules.ExplicitPath = path
	} else if home, err := os.UserHomeDir(); err == nil {
		loadingRules.Precedence = []string{filepath.Join(home, ".kube", "config")}
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
}

func NewClients(config *rest.Config) (*Clients, error) {
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	coreClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &Clients{Dynamic: dynamicClient, Core: coreClient}, nil
}
