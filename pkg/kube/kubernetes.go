package kube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/pkg/client/clientset/versioned"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientauthapi "k8s.io/client-go/tools/clientcmd/api"
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

// RestConfigFromCluster creates a rest.Config from a cluster
func RestConfigFromCluster(c *v1alpha1.Cluster, kubeconfigPath string) (*rest.Config, error) {
	tls := rest.TLSClientConfig{
		Insecure:   c.Config.Insecure,
		ServerName: c.Config.ServerName,
		CertData:   c.Config.CertData,
		KeyData:    c.Config.KeyData,
		CAData:     c.Config.CAData,
	}

	var cfg *rest.Config

	// if the server is in-cluster, load the kubeconfig from the kubeconfig file or the default kubeconfig file
	if strings.Contains(c.Server, "kubernetes.default.svc") {
		localCfg, err := rest.InClusterConfig()
		if err != nil && !errors.Is(err, rest.ErrNotInCluster) {
			return nil, fmt.Errorf("failed to create config: %v", err)
		}
		if localCfg == nil {
			if kubeconfigPath != "" {
				localCfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
			} else {
				loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
				configOverrides := &clientcmd.ConfigOverrides{}
				kubeCfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
				localCfg, err = kubeCfg.ClientConfig()
			}
			if err != nil {
				return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
			}
		}
		cfg = localCfg
	} else {

		switch {
		case c.Config.AWSAuthConfig != nil:
			// EKS via argocd-k8s-auth (same contract as Argo CD)
			args := []string{"aws", "--cluster-name", c.Config.AWSAuthConfig.ClusterName}
			if c.Config.AWSAuthConfig.RoleARN != "" {
				args = append(args, "--role-arn", c.Config.AWSAuthConfig.RoleARN)
			}
			if c.Config.AWSAuthConfig.Profile != "" {
				args = append(args, "--profile", c.Config.AWSAuthConfig.Profile)
			}
			cfg = &rest.Config{
				Host:            c.Server,
				TLSClientConfig: tls,
				ExecProvider: &clientauthapi.ExecConfig{
					APIVersion:      "client.authentication.k8s.io/v1beta1",
					Command:         "argocd-k8s-auth",
					Args:            args,
					InteractiveMode: clientauthapi.NeverExecInteractiveMode,
				},
			}

		case c.Config.ExecProviderConfig != nil:
			// Generic exec provider (OIDC, SSO, etc.)
			var env []clientauthapi.ExecEnvVar
			for k, v := range c.Config.ExecProviderConfig.Env {
				env = append(env, clientauthapi.ExecEnvVar{Name: k, Value: v})
			}
			cfg = &rest.Config{
				Host:            c.Server,
				TLSClientConfig: tls,
				ExecProvider: &clientauthapi.ExecConfig{
					APIVersion:      c.Config.ExecProviderConfig.APIVersion,
					Command:         c.Config.ExecProviderConfig.Command,
					Args:            c.Config.ExecProviderConfig.Args,
					Env:             env,
					InstallHint:     c.Config.ExecProviderConfig.InstallHint,
					InteractiveMode: clientauthapi.NeverExecInteractiveMode,
				},
			}

		default:
			// Static auth (token or basic) and TLS
			cfg = &rest.Config{
				Host:            c.Server,
				Username:        c.Config.Username,
				Password:        c.Config.Password,
				BearerToken:     c.Config.BearerToken,
				TLSClientConfig: tls,
			}
		}
	}

	if c.Config.ProxyUrl != "" {
		u, err := v1alpha1.ParseProxyUrl(c.Config.ProxyUrl)
		if err != nil {
			return nil, fmt.Errorf("failed to parse proxy url: %w", err)
		}
		cfg.Proxy = http.ProxyURL(u)
	}
	// Apply Argo CD defaults
	cfg.DisableCompression = c.Config.DisableCompression
	cfg.Timeout = v1alpha1.K8sServerSideTimeout
	cfg.QPS = v1alpha1.K8sClientConfigQPS
	cfg.Burst = v1alpha1.K8sClientConfigBurst
	v1alpha1.SetK8SConfigDefaults(cfg)

	return cfg, nil
}
