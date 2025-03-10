package argocd

import (
	"context"
	"fmt"
	"os"

	"github.com/anandf/resource-tracker/pkg/kube"
	"github.com/argoproj/argo-cd/v2/common"
	argocdclient "github.com/argoproj/argo-cd/v2/pkg/apiclient"
	"github.com/argoproj/argo-cd/v2/pkg/apiclient/application"
	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v2/reposerver/apiclient"
	"github.com/argoproj/argo-cd/v2/util/db"
	kubeutil "github.com/argoproj/argo-cd/v2/util/kube"
	"github.com/argoproj/argo-cd/v2/util/settings"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

// Native
type argoCD struct {
	Client argocdclient.Client
}

// ArgoCD is the interface for accessing Argo CD functions we need
type ArgoCD interface {
	ListApplications() ([]v1alpha1.Application, error)
}

type ResourceTrackerResult struct {
	NumApplicationsProcessed int
	NumErrors                int
}

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

func FilterApplicationsByArgoCDNamespace(apps []v1alpha1.Application, namespace string) []v1alpha1.Application {
	var filteredApps []v1alpha1.Application
	for _, app := range apps {
		if app.Status.ControllerNamespace == namespace {
			filteredApps = append(filteredApps, app)
		}
	}
	return filteredApps
}

// ListApplications returns a list of all application names that the API user
// has access to.
func (client *argoCD) ListApplications() ([]v1alpha1.Application, error) {
	conn, appClient, err := client.Client.NewApplicationClient()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	apps, err := appClient.List(context.TODO(), &application.ApplicationQuery{})
	if err != nil {
		return nil, err
	}
	return apps.Items, nil
}

// Kubernetes based client
type k8sClient struct {
	kubeClient *kube.ResourceTrackerKubeClient
}

// ListApplications lists all applications across all namespaces.
func (client *k8sClient) ListApplications() ([]v1alpha1.Application, error) {
	list, err := client.kubeClient.ApplicationClientSet.ArgoprojV1alpha1().Applications(v1.NamespaceAll).List(context.TODO(), v1.ListOptions{})
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

func ProcessApplication(app v1alpha1.Application, cfg *kube.ResourceTrackerKubeClient) error {
	// Fetch resource-relation-lookup ConfigMap
	configMap, err := cfg.KubeClient.Clientset.CoreV1().ConfigMaps(app.Status.ControllerNamespace).Get(
		context.Background(), "resource-relation-lookup", v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to fetch resource-relation-lookup ConfigMap: %w", err)
	}
	resourceRelations := configMap.Data
	// Fetch AppProject
	appProject, err := cfg.ApplicationClientSet.ArgoprojV1alpha1().AppProjects(app.Status.ControllerNamespace).Get(
		context.Background(), app.Spec.Project, v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to fetch AppProject %s: %w", app.Spec.Project, err)
	}
	// Get RepoServer address
	repoServerAddress, err := getRepoServerAddress(cfg.KubeClient.Clientset, app.Status.ControllerNamespace)
	if err != nil {
		return fmt.Errorf("failed to get repo server address: %w", err)
	}
	// Initialize required services
	settingsManager := settings.NewSettingsManager(context.Background(), cfg.KubeClient.Clientset, app.Status.ControllerNamespace)
	db := db.NewDB(app.Status.ControllerNamespace, settingsManager, cfg.KubeClient.Clientset)
	repoClientset := apiclient.NewRepoServerClientset(repoServerAddress, 120, apiclient.TLSConfiguration{DisableTLS: false, StrictValidation: false})
	// Get child manifests
	targetObjs, destinationConfig, err := GetApplicationChildManifests(context.Background(), &app, appProject, app.Status.ControllerNamespace, db, settingsManager, repoClientset, kubeutil.NewKubectl())
	if err != nil {
		return fmt.Errorf("failed to get application child manifests: %w", err)
	}
	// Check if all required resources exist in the current resourceRelations map
	needsUpdate := false
	for _, obj := range targetObjs {
		resourceKey := kube.GetResourceKey(obj.GetAPIVersion(), obj.GetKind())
		if _, exists := resourceRelations[resourceKey]; !exists {
			needsUpdate = true
			break
		}
	}
	// If missing resources are found, update the resource relations
	if needsUpdate {
		resourceRelations, err = updateResourceRelationLookup(destinationConfig, app.Status.ControllerNamespace, cfg.KubeClient.Clientset)
		if err != nil {
			klog.Errorf("failed to update resource-relation-lookup ConfigMap: %v", err)
			return err
		}
	}
	// Discover parent-child relationships
	parentChildMap := kube.GetResourceRelation(resourceRelations, targetObjs)
	return UpdateResourceInclusion(parentChildMap, cfg.KubeClient.Clientset, app.Status.ControllerNamespace)
}

func getRepoServerAddress(k8sclient kubernetes.Interface, controllerNamespace string) (string, error) {
	labelSelector := fmt.Sprintf("%s=%s", common.LabelKeyComponentRepoServer, common.LabelValueComponentRepoServer)
	serviceList, err := k8sclient.CoreV1().Services(controllerNamespace).List(context.Background(), v1.ListOptions{LabelSelector: labelSelector})
	if err != nil || len(serviceList.Items) == 0 {
		return "", fmt.Errorf("failed to find repo server service: %w", err)
	}
	repoServerName, ok := serviceList.Items[0].Labels[common.LabelKeyAppName]
	if !ok || repoServerName == "" {
		return "", fmt.Errorf("repo server name label missing")
	}
	port, err := kubeutil.PortForward(8081, controllerNamespace, &clientcmd.ConfigOverrides{}, common.LabelKeyAppName+"="+repoServerName)
	if err != nil {
		return "", fmt.Errorf("failed to port-forward repo server: %w", err)
	}
	return fmt.Sprintf("localhost:%d", port), nil
}
