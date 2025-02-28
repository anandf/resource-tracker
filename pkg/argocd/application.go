package argocd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/anandf/resource-tracker/pkg/kube"
	argocdclient "github.com/argoproj/argo-cd/v2/pkg/apiclient"
	"github.com/argoproj/argo-cd/v2/pkg/apiclient/application"
	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Native
type argoCD struct {
	Client argocdclient.Client
}

// ArgoCD is the interface for accessing Argo CD functions we need
type ArgoCD interface {
	GetApplication(ctx context.Context, appName string) (*v1alpha1.Application, error)
	ListApplications(labelSelector string) ([]v1alpha1.Application, error)
}

type ResourceTrackerResult struct{}

// Basic wrapper struct for ArgoCD client options
type ClientOptions struct {
	ServerAddr      string
	Insecure        bool
	Plaintext       bool
	Certfile        string
	GRPCWeb         bool
	GRPCWebRootPath string
	AuthToken       string
}

// NewAPIClient creates a new API client for ArgoCD and connects to the ArgoCD Server
func NewAPIClient(opts *ClientOptions) (ArgoCD, error) {

	envAuthToken := os.Getenv("ARGOCD_TOKEN")
	if envAuthToken != "" && opts.AuthToken == "" {
		opts.AuthToken = envAuthToken
	}

	rOpts := argocdclient.ClientOptions{
		ServerAddr:      opts.ServerAddr,
		PlainText:       opts.Plaintext,
		Insecure:        opts.Insecure,
		CertFile:        opts.Certfile,
		GRPCWeb:         opts.GRPCWeb,
		GRPCWebRootPath: opts.GRPCWebRootPath,
		AuthToken:       opts.AuthToken,
	}
	client, err := argocdclient.NewClient(&rOpts)
	if err != nil {
		return nil, err
	}
	return &argoCD{Client: client}, nil
}

// GetApplication gets the application named appName from Argo CD API
func (client *argoCD) GetApplication(ctx context.Context, appName string) (*v1alpha1.Application, error) {
	conn, appClient, err := client.Client.NewApplicationClient()
	defer conn.Close()
	app, err := appClient.Get(ctx, &application.ApplicationQuery{Name: &appName})
	if err != nil {
		return nil, err
	}
	return app, nil
}

// ListApplications returns a list of all application names that the API user
// has access to.
func (client *argoCD) ListApplications(labelSelector string) ([]v1alpha1.Application, error) {
	conn, appClient, err := client.Client.NewApplicationClient()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	apps, err := appClient.List(context.TODO(), &application.ApplicationQuery{Selector: &labelSelector})
	if err != nil {
		return nil, err
	}
	return apps.Items, nil
}

// Kubernetes based client
type k8sClient struct {
	kubeClient *kube.ResourceTrackerKubeClient
}

// GetApplication retrieves an application by name across all namespaces.
func (client *k8sClient) GetApplication(ctx context.Context, appName string) (*v1alpha1.Application, error) {
	// List all applications across all namespaces (using empty labelSelector)
	appList, err := client.ListApplications(v1.NamespaceAll)
	if err != nil {
		return nil, fmt.Errorf("error listing applications: %w", err)
	}

	// Filter applications by name using nameMatchesPattern
	var matchedApps []v1alpha1.Application

	for _, app := range appList {
		log.Debugf("Found application: %s in namespace %s", app.Name, app.Namespace)
		if nameMatchesPattern(app.Name, []string{appName}) {
			log.Debugf("Application %s matches the pattern", app.Name)
			matchedApps = append(matchedApps, app)
		}
	}

	if len(matchedApps) == 0 {
		return nil, fmt.Errorf("application %s not found", appName)
	}

	if len(matchedApps) > 1 {
		return nil, fmt.Errorf("multiple applications found matching %s", appName)
	}

	// Retrieve the application in the specified namespace
	return &matchedApps[0], nil
}

// ListApplications lists all applications across all namespaces.
func (client *k8sClient) ListApplications(labelSelector string) ([]v1alpha1.Application, error) {
	list, err := client.kubeClient.ApplicationClientSet.ArgoprojV1alpha1().Applications(v1.NamespaceAll).List(context.TODO(), v1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, fmt.Errorf("error listing applications: %w", err)
	}
	log.Debugf("Applications listed: %d", len(list.Items))
	return list.Items, nil
}

// NewK8SClient creates a new kube client to interact with kube api-server.
func NewK8SClient(kubeClient *kube.ResourceTrackerKubeClient) (ArgoCD, error) {
	return &k8sClient{kubeClient: kubeClient}, nil
}

// Match a name against a list of patterns
func nameMatchesPattern(name string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		log.Tracef("Matching application name %s against pattern %s", name, p)
		if m, err := filepath.Match(p, name); err != nil {
			log.Warnf("Invalid application name pattern '%s': %v", p, err)
		} else if m {
			return true
		}
	}
	return false
}
