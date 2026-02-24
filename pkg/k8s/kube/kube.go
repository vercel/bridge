package kube

import (
	"log/slog"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// apiTimeout is the default timeout for Kubernetes API requests made via
// rest.Config. This prevents the CLI from hanging when the API server is
// unreachable.
const apiTimeout = 10 * time.Second

// Config holds optional overrides for out-of-cluster kubeconfig resolution.
type Config struct {
	// Context overrides the current kubectl context. Ignored in-cluster.
	Context string
	// Namespace overrides the default namespace from the kubeconfig. Ignored in-cluster.
	Namespace string
}

// RestConfig resolves a *rest.Config. It tries in-cluster config first
// and falls back to the default kubeconfig, logging a warning with the active
// kubectl context when running out-of-cluster.
func RestConfig(cfg Config) (*rest.Config, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		slog.Warn("Not running in-cluster, falling back to kubeconfig")

		overrides := &clientcmd.ConfigOverrides{}
		if cfg.Context != "" {
			overrides.CurrentContext = cfg.Context
		}
		if cfg.Namespace != "" {
			overrides.Context.Namespace = cfg.Namespace
		}

		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)

		raw, rawErr := kubeConfig.RawConfig()
		if rawErr == nil {
			slog.Warn("Using kubectl context", "context", raw.CurrentContext)
		}

		config, err = kubeConfig.ClientConfig()
		if err != nil {
			return nil, err
		}
	}

	if config.Timeout == 0 {
		config.Timeout = apiTimeout
	}

	return config, nil
}

// NewClientset builds a Kubernetes clientset using RestConfig.
func NewClientset(cfg Config) (kubernetes.Interface, error) {
	config, err := RestConfig(cfg)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}
