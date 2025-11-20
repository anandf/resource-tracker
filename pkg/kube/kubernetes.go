package kube

import (
	"context"
	"errors"
	"fmt"

	"github.com/argoproj/argo-cd/v3/pkg/client/clientset/versioned"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type ResourceTrackerKubeClient struct {
	KubeClient           *KubeClient
	ApplicationClientSet versioned.Interface
}

type KubeClient struct {
	Clientset kubernetes.Interface
	Context   context.Context
	Namespace string
}

func NewKubernetesClient(ctx context.Context, client kubernetes.Interface, namespace string) *KubeClient {
	kc := &KubeClient{}
	kc.Context = ctx
	kc.Clientset = client
	kc.Namespace = namespace
	return kc
}

// NewKubernetesClientFromConfig creates a new Kubernetes client object from given
// rest.Config object.
func NewKubernetesClientFromConfig(ctx context.Context, namespace string, kubeConfig *rest.Config) (*ResourceTrackerKubeClient, error) {
	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return nil, err
	}
	applicationsClientset, err := versioned.NewForConfig(kubeConfig)
	if err != nil {
		return nil, err
	}

	kc := &ResourceTrackerKubeClient{}
	kc.ApplicationClientSet = applicationsClientset
	kc.KubeClient = NewKubernetesClient(ctx, clientset, namespace)
	return kc, nil
}

func GetKubeConfig(kubeconfigPath string) (*rest.Config, error) {
	var restConfig *rest.Config
	// If caller did not provide a kubeconfig, try in-cluster config first

	restConfig, err := rest.InClusterConfig()
	if err != nil && !errors.Is(err, rest.ErrNotInCluster) {
		return nil, fmt.Errorf("failed to create config: %v", err)
	}

	// If the binary is not being run inside a kubernetes cluster,
	// nor the caller provided a kubeconfig,
	// try loading the config from KUBECONFIG env or $HOME/.kube/config file
	if restConfig == nil {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		loadingRules.DefaultClientConfig = &clientcmd.DefaultClientConfig
		loadingRules.ExplicitPath = kubeconfigPath
		configOverrides := &clientcmd.ConfigOverrides{}
		kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
		restConfig, err = kubeConfig.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to create config: %v", err)
		}
	}
	return restConfig, nil
}
