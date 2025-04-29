package argocd

import (
	"context"
	"fmt"
	"sync"

	"github.com/anandf/resource-tracker/pkg/kube"
	"github.com/anandf/resource-tracker/pkg/resourcegraph"
	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v2/pkg/client/clientset/versioned"
	"github.com/emirpasic/gods/sets/hashset"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// ArgoCD is the interface for accessing Argo CD functions we need
type ArgoCD interface {
	ListApplications() ([]v1alpha1.Application, error)
	ProcessApplication(v1alpha1.Application, string) (map[string]*hashset.Set, error)
	FilterApplicationsByArgoCDNamespace([]v1alpha1.Application, string) []v1alpha1.Application
	PopulateTrackedResources(map[string]*hashset.Set)
	UpdateResourceInclusion(string) error
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
	applicationClientSet versioned.Interface
	resourceMapperStore  map[string]*resourcegraph.ResourceMapper
	repoServer           *repoServerManager
	trackedResources     map[string]*hashset.Set
	mapperMutex          sync.Mutex
}

// ListApplications lists all applications across all namespaces.
func (a *argocd) ListApplications() ([]v1alpha1.Application, error) {
	list, err := a.applicationClientSet.ArgoprojV1alpha1().Applications(v1.NamespaceAll).List(context.TODO(), v1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("error listing applications: %w", err)
	}
	return list.Items, nil
}

// NewK8SClient creates a new kube client to interact with kube api-server.
func NewArgocd(kubeClient *kube.ResourceTrackerKubeClient, repoServerAddress string, repoServerTimeoutSeconds int, repoServerPlaintext bool, repoServerStrictTLS bool) (ArgoCD, error) {
	repoServer := NewRepoServerManager(kubeClient.KubeClient.Clientset, kubeClient.KubeClient.Namespace, repoServerAddress, repoServerTimeoutSeconds, repoServerPlaintext, repoServerStrictTLS)
	return &argocd{kubeClient: kubeClient.KubeClient, applicationClientSet: kubeClient.ApplicationClientSet, repoServer: repoServer}, nil
}

// ProcessApplication processes a list of applications and updates resource inclusions.
func (a *argocd) ProcessApplication(app v1alpha1.Application, argocdNamespace string) (map[string]*hashset.Set, error) {
	ctx := context.Background()
	if argocdNamespace == "" {
		argocdNamespace = a.kubeClient.Namespace
	}
	//Collect all applications resource relations
	log.Infof("Processing application: %s", app.Name)
	// Fetch AppProject
	appProject, err := a.applicationClientSet.ArgoprojV1alpha1().AppProjects(argocdNamespace).Get(ctx, app.Spec.Project, v1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch AppProject %s for application %s: %w", app.Spec.Project, app.Name, err)
	}
	log.Infof("Fetched AppProject: %s for application: %s", app.Spec.Project, app.Name)
	// Get target object from repo-server
	targetObjs, destinationConfig, err := getApplicationChildManifests(ctx, &app, appProject, argocdNamespace, a.repoServer)
	if err != nil {
		return nil, fmt.Errorf("failed to get application child manifests for %s: %w", app.Name, err)
	}
	log.Infof("Fetched target manifests from repo-server for application: %s", app.Name)
	// Get or create resource mapper
	err = a.syncResourceMapper(destinationConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to get resource mapper for application %s: %w", app.Name, err)
	}
	// Update resource relation lookup if required
	resourceRelations, err := a.updateResourceRelationLookup(argocdNamespace, destinationConfig.Host, app.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to update resource relation lookup for %s: %w", app.Name, err)
	}
	// Discover parent-child relationships and group resources
	parentChildMap := kube.GetResourceRelation(resourceRelations, targetObjs)
	log.Infof("Discovered parent-child relationships: %v for application: %s", parentChildMap, app.Name)
	return groupResourcesByAPIGroup(parentChildMap), nil
}

func (a *argocd) syncResourceMapper(destinationConfig *rest.Config) error {
	a.mapperMutex.Lock() // Lock before accessing shared map
	defer a.mapperMutex.Unlock()

	if a.resourceMapperStore == nil {
		a.resourceMapperStore = make(map[string]*resourcegraph.ResourceMapper)
	}
	if _, exists := a.resourceMapperStore[destinationConfig.Host]; !exists {
		mapper, err := resourcegraph.NewResourceMapper(destinationConfig)
		if err != nil {
			return fmt.Errorf("failed to create ResourceMapper: %w", err)
		}
		go mapper.StartInformer()
		a.resourceMapperStore[destinationConfig.Host] = mapper
	}
	return nil
}

func (a *argocd) PopulateTrackedResources(groupedResources map[string]*hashset.Set) {
	if a.trackedResources == nil {
		a.trackedResources = make(map[string]*hashset.Set)
	}
	for group, kinds := range groupedResources {
		if _, exists := a.trackedResources[group]; !exists {
			a.trackedResources[group] = hashset.New()
		}
		for _, resource := range kinds.Values() {
			a.trackedResources[group].Add(resource)
		}
	}
}
