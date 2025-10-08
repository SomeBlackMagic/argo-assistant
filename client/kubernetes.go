// Package client provides Kubernetes client creation and configuration
package client

import (
	"os"

	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// CreateKubernetesClient initializes and returns a Kubernetes clientset and rest config.
// It first tries to use in-cluster configuration, falling back to KUBECONFIG if that fails.
func CreateKubernetesClient(logger *logrus.Logger) (*kubernetes.Clientset, *rest.Config, error) {
	logger.Info("Initializing Kubernetes client...")

	// Try InCluster config first, then fall back to KUBECONFIG
	cfg, err := rest.InClusterConfig()
	if err != nil {
		logger.Debug("Failed to load in-cluster config, trying KUBECONFIG...")
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			if home, _ := os.UserHomeDir(); home != "" {
				kubeconfig = home + "/.kube/config"
			}
		}
		logger.WithField("kubeconfig", kubeconfig).Debug("Using kubeconfig file")
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			logger.WithError(err).Error("Failed to build config from kubeconfig")
			return nil, nil, err
		}
	} else {
		logger.Info("Successfully loaded in-cluster configuration")
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.WithError(err).Error("Failed to create Kubernetes clientset")
		return nil, nil, err
	}

	logger.Info("Kubernetes client successfully initialized")
	return cs, cfg, nil
}
