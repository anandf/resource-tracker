package kube

import (
	"context"
	"os"

	"github.com/argoproj/argo-cd/v2/pkg/client/clientset/versioned"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type ResourceTrackerKubeClient struct {
	KubeClient           KubeClient
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

// NewKubernetesClient creates a new Kubernetes client object from given
// configuration file. If configuration file is the empty string, in-cluster
// client will be created.
func NewKubernetesClientFromConfig(ctx context.Context, namespace string, kubeconfig string) (*KubeClient, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.DefaultClientConfig = &clientcmd.DefaultClientConfig
	loadingRules.ExplicitPath = kubeconfig
	overrides := clientcmd.ConfigOverrides{}
	clientConfig := clientcmd.NewInteractiveDeferredLoadingClientConfig(loadingRules, &overrides, os.Stdin)

	config, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, err
	}

	if namespace == "" {
		namespace, _, err = clientConfig.Namespace()
		if err != nil {
			return nil, err
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return NewKubernetesClient(ctx, clientset, namespace), nil
}
