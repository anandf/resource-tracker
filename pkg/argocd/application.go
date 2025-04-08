package argocd

import (
	"context"
	"fmt"

	"slices"

	"github.com/anandf/resource-tracker/pkg/kube"
	"github.com/anandf/resource-tracker/pkg/resourcegraph"
	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v2/pkg/client/clientset/versioned"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// ArgoCD is the interface for accessing Argo CD functions we need
type ArgoCD interface {
	ListApplications() ([]v1alpha1.Application, error)
	ProcessApplication([]v1alpha1.Application, string) []error
	FilterApplicationsByArgoCDNamespace([]v1alpha1.Application, string) []v1alpha1.Application
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
}

// ListApplications lists all applications across all namespaces.
func (a *argocd) ListApplications() ([]v1alpha1.Application, error) {
	list, err := a.applicationClientSet.ArgoprojV1alpha1().Applications(v1.NamespaceAll).List(context.TODO(), v1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("error listing applications: %w", err)
	}
	log.Infof("Successfully listed %d applications", len(list.Items))
	return list.Items, nil
}

// NewK8SClient creates a new kube client to interact with kube api-server.
func NewArgocd(kubeClient *kube.ResourceTrackerKubeClient, repoServerAddress string, repoServerTimeoutSeconds int, repoServerPlaintext bool, repoServerStrictTLS bool) (ArgoCD, error) {
	repoServer := NewRepoServerManager(kubeClient.KubeClient.Clientset, kubeClient.KubeClient.Namespace, repoServerAddress, repoServerTimeoutSeconds, repoServerPlaintext, repoServerStrictTLS)
	return &argocd{kubeClient: kubeClient.KubeClient, applicationClientSet: kubeClient.ApplicationClientSet, repoServer: repoServer}, nil
}

// ProcessApplication processes a list of applications and updates resource inclusions.
func (a *argocd) ProcessApplication(apps []v1alpha1.Application, argocdNamespace string) []error {
	ctx := context.Background()                   // Single context for the entire process
	groupedResources := make(map[string][]string) // Consolidated resource map for all applications
	var errorList []error                         // Collect individual errors
	if argocdNamespace == "" {
		argocdNamespace = a.kubeClient.Namespace
	}
	//Collect all applications resource relations
	//and update resource inclusion arfer processing all applications.
	for _, app := range apps {
		log.Infof("Processing application: %s", app.Name)
		// Fetch AppProject
		appProject, err := a.applicationClientSet.ArgoprojV1alpha1().AppProjects(argocdNamespace).Get(ctx, app.Spec.Project, v1.GetOptions{})
		if err != nil {
			errMsg := fmt.Errorf("failed to fetch AppProject %s for application %s: %w", app.Spec.Project, app.Name, err)
			log.Error(errMsg)
			errorList = append(errorList, errMsg)
			continue
		}
		log.Infof("Fetched AppProject: %s for application: %s", app.Spec.Project, app.Name)
		// Get target object from repo-server
		targetObjs, destinationConfig, err := getApplicationChildManifests(ctx, &app, appProject, argocdNamespace, a.repoServer)
		if err != nil {
			errMsg := fmt.Errorf("failed to get application child manifests for %s: %w", app.Name, err)
			log.Error(errMsg)
			errorList = append(errorList, errMsg)
			continue
		}
		log.Infof("Fetched target manifests from repo-server for application: %s", app.Name)
		// Get or create resource mapper
		mapper, err := a.getOrCreateResourceMapper(destinationConfig)
		if err != nil {
			errMsg := fmt.Errorf("failed to get resource mapper for application %s: %w", app.Name, err)
			log.Error(errMsg)
			errorList = append(errorList, errMsg)
			continue
		}
		// Update resource relation lookup if required
		resourceRelations, err := updateResourceRelationLookup(mapper, argocdNamespace, a.kubeClient.Clientset, app.Name)
		if err != nil {
			errMsg := fmt.Errorf("failed to update resource relation lookup for %s: %w", app.Name, err)
			log.Error(errMsg)
			errorList = append(errorList, errMsg)
			continue
		}
		// Discover parent-child relationships and group resources
		parentChildMap := kube.GetResourceRelation(resourceRelations, targetObjs)
		log.Infof("Discovered parent-child relationships: %v for application: %s", parentChildMap, app.Name)
		appGroupedResources := groupResourcesByAPIGroup(parentChildMap)
		// Merge `appGroupedResources` into `groupedResources`
		for apiGroup, kinds := range appGroupedResources {
			if _, exists := groupedResources[apiGroup]; !exists {
				groupedResources[apiGroup] = []string{}
			}
			for _, kind := range kinds {
				if !slices.Contains(groupedResources[apiGroup], kind) {
					groupedResources[apiGroup] = append(groupedResources[apiGroup], kind)
				}
			}
		}
	}
	// If no grouped resources were collected, return early
	if len(groupedResources) == 0 {
		log.Warn("No valid grouped resources found, skipping resource inclusion update.")
		return errorList
	}
	log.Info("Updating resource inclusions in argocd-cm ConfigMap...")
	// Update resource inclusions if required
	if err := updateresourceInclusion(groupedResources, a.kubeClient.Clientset, argocdNamespace); err != nil {
		errMsg := fmt.Errorf("failed to update resource inclusions: %w", err)
		log.Error(errMsg)
		errorList = append(errorList, errMsg)
	}
	return errorList
}

func (a *argocd) getOrCreateResourceMapper(destinationConfig *rest.Config) (*resourcegraph.ResourceMapper, error) {
	if a.resourceMapperStore == nil {
		a.resourceMapperStore = make(map[string]*resourcegraph.ResourceMapper)
	}
	mapper, exists := a.resourceMapperStore[destinationConfig.Host]
	if !exists {
		var err error
		mapper, err = resourcegraph.NewResourceMapper(destinationConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create ResourceMapper: %w", err)
		}
		go mapper.StartInformer()
		a.resourceMapperStore[destinationConfig.Host] = mapper
	}
	return mapper, nil
}
