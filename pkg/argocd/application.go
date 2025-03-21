package argocd

import (
	"context"
	"fmt"

	"github.com/anandf/resource-tracker/pkg/kube"
	"github.com/anandf/resource-tracker/pkg/resourcegraph"
	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v2/pkg/client/clientset/versioned"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

// ArgoCD is the interface for accessing Argo CD functions we need
type ArgoCD interface {
	ListApplications() ([]v1alpha1.Application, error)
	ProcessApplication(v1alpha1.Application) error
	FilterApplicationsByArgoCDNamespace([]v1alpha1.Application, string) []v1alpha1.Application
}

type ResourceTrackerResult struct {
	NumApplicationsProcessed int
	NumErrors                int
}

func (argocd *argocd) FilterApplicationsByArgoCDNamespace(apps []v1alpha1.Application, namespace string) []v1alpha1.Application {
	if namespace == "" {
		namespace = argocd.kubeClient.Namespace
	}
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
	repoServer           *repoServerManager
}

// ListApplications lists all applications across all namespaces.
func (a *argocd) ListApplications() ([]v1alpha1.Application, error) {
	list, err := a.ApplicationClientSet.ArgoprojV1alpha1().Applications(v1.NamespaceAll).List(context.TODO(), v1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("error listing applications: %w", err)
	}
	log.Debugf("Applications listed: %d", len(list.Items))
	return list.Items, nil
}

// NewK8SClient creates a new kube client to interact with kube api-server.
func NewArgocd(kubeClient *kube.ResourceTrackerKubeClient, repoServerAddress string, repoServerTimeoutSeconds int, repoServerPlaintext bool, repoServerStrictTLS bool) (ArgoCD, error) {
	repoServer := NewRepoServerManager(kubeClient.KubeClient.Clientset, kubeClient.KubeClient.Namespace, repoServerAddress, repoServerTimeoutSeconds, repoServerPlaintext, repoServerStrictTLS)
	return &argocd{kubeClient: kubeClient.KubeClient, ApplicationClientSet: kubeClient.ApplicationClientSet, repoServer: repoServer}, nil
}

func (a *argocd) ProcessApplication(app v1alpha1.Application) error {
	// Fetch resource-relation-lookup ConfigMap
	configMap, err := a.kubeClient.Clientset.CoreV1().ConfigMaps(app.Status.ControllerNamespace).Get(
		context.Background(), "resource-relation-lookup", v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to fetch resource-relation-lookup ConfigMap: %w", err)
	}
	resourceRelations := configMap.Data
	// Fetch AppProject
	appProject, err := a.ApplicationClientSet.ArgoprojV1alpha1().AppProjects(app.Status.ControllerNamespace).Get(
		context.Background(), app.Spec.Project, v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to fetch AppProject %s: %w", app.Spec.Project, err)
	}
	// Get child manifests
	targetObjs, destinationConfig, err := getApplicationChildManifests(context.Background(), &app, appProject, app.Status.ControllerNamespace, a.repoServer)
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
		mapper, err := a.getOrCreateResourceMapper(destinationConfig)
		if err != nil {
			klog.Errorf("Failed to get resource mapper: %v", err)
			return err
		}

		resourceRelations, err = updateResourceRelationLookup(mapper, app.Status.ControllerNamespace, a.kubeClient.Clientset)
		if err != nil {
			klog.Errorf("failed to update resource-relation-lookup ConfigMap: %v", err)
			return err
		}
	}
	// Discover parent-child relationships
	parentChildMap := kube.GetResourceRelation(resourceRelations, targetObjs)
	return updateresourceInclusion(parentChildMap, a.kubeClient.Clientset, app.Status.ControllerNamespace)
}

func (a *argocd) getOrCreateResourceMapper(destinationConfig *rest.Config) (*resourcegraph.ResourceMapper, error) {
	if a.resourceMapperStore == nil {
		a.resourceMapperStore = make(map[string]*resourcegraph.ResourceMapper)
	}
	mapper, exists := a.resourceMapperStore[destinationConfig.ServerName]
	if !exists {
		var err error
		mapper, err = resourcegraph.NewResourceMapper(destinationConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create ResourceMapper: %w", err)
		}
		// Start informer
		go mapper.StartInformer()
		a.resourceMapperStore[destinationConfig.ServerName] = mapper
	}
	return mapper, nil
}
