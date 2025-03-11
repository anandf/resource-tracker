package argocd

import (
	"context"
	"fmt"

	"github.com/anandf/resource-tracker/pkg/kube"
	"github.com/anandf/resource-tracker/pkg/resourcegraph"
	"github.com/argoproj/argo-cd/v2/common"
	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v2/pkg/client/clientset/versioned"
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

// ArgoCD is the interface for accessing Argo CD functions we need
type ArgoCD interface {
	ListApplications() ([]v1alpha1.Application, error)
	ProcessApplication(v1alpha1.Application) error
}

type ResourceTrackerResult struct {
	NumApplicationsProcessed int
	NumErrors                int
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

// Kubernetes based client
type argocd struct {
	kubeClient           *kube.KubeClient
	ApplicationClientSet versioned.Interface
	resourceMapperStore  map[string]*resourcegraph.ResourceMapper
}

// ListApplications lists all applications across all namespaces.
func (argocd *argocd) ListApplications() ([]v1alpha1.Application, error) {
	list, err := argocd.ApplicationClientSet.ArgoprojV1alpha1().Applications(v1.NamespaceAll).List(context.TODO(), v1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("error listing applications: %w", err)
	}
	log.Debugf("Applications listed: %d", len(list.Items))
	return list.Items, nil
}

// NewK8SClient creates a new kube client to interact with kube api-server.
func NewK8SClient(kubeClient *kube.ResourceTrackerKubeClient) (ArgoCD, error) {
	return &argocd{kubeClient: kubeClient.KubeClient, ApplicationClientSet: kubeClient.ApplicationClientSet}, nil
}

func (argocd *argocd) ProcessApplication(app v1alpha1.Application) error {
	// Fetch resource-relation-lookup ConfigMap
	configMap, err := argocd.kubeClient.Clientset.CoreV1().ConfigMaps(app.Status.ControllerNamespace).Get(
		context.Background(), "resource-relation-lookup", v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to fetch resource-relation-lookup ConfigMap: %w", err)
	}
	resourceRelations := configMap.Data
	// Fetch AppProject
	appProject, err := argocd.ApplicationClientSet.ArgoprojV1alpha1().AppProjects(app.Status.ControllerNamespace).Get(
		context.Background(), app.Spec.Project, v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to fetch AppProject %s: %w", app.Spec.Project, err)
	}
	// Get RepoServer address
	repoServerAddress, err := getRepoServerAddress(argocd.kubeClient.Clientset, app.Status.ControllerNamespace)
	if err != nil {
		return fmt.Errorf("failed to get repo server address: %w", err)
	}
	// Initialize required services
	settingsManager := settings.NewSettingsManager(context.Background(), argocd.kubeClient.Clientset, app.Status.ControllerNamespace)
	db := db.NewDB(app.Status.ControllerNamespace, settingsManager, argocd.kubeClient.Clientset)
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
		if argocd.resourceMapperStore == nil {
			argocd.resourceMapperStore = make(map[string]*resourcegraph.ResourceMapper)
		}
		mapper, exists := argocd.resourceMapperStore[destinationConfig.ServerName]
		if !exists {
			var err error
			mapper, err = resourcegraph.NewResourceMapper(destinationConfig)
			if err != nil {
				return fmt.Errorf("failed to create ResourceMapper: %w", err)
			}
			argocd.resourceMapperStore[destinationConfig.ServerName] = mapper
		}
		if mapper.RequireInit() {
			if err := mapper.Init(); err != nil {
				return fmt.Errorf("failed to initialize ResourceMapper: %w", err)
			}
		}
		resourceRelations, err = updateResourceRelationLookup(mapper, app.Status.ControllerNamespace, argocd.kubeClient.Clientset)
		if err != nil {
			klog.Errorf("failed to update resource-relation-lookup ConfigMap: %v", err)
			return err
		}
	}
	// Discover parent-child relationships
	parentChildMap := kube.GetResourceRelation(resourceRelations, targetObjs)
	return UpdateResourceInclusion(parentChildMap, argocd.kubeClient.Clientset, app.Status.ControllerNamespace)
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
