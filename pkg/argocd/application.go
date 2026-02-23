package argocd

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/pkg/client/clientset/versioned"
	"github.com/argoproj/argo-cd/v3/util/db"
	"github.com/argoproj/argo-cd/v3/util/settings"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"

	"github.com/anandf/resource-tracker/pkg/common"
	"github.com/anandf/resource-tracker/pkg/graph"
	"github.com/anandf/resource-tracker/pkg/kube"
	"github.com/anandf/resource-tracker/pkg/repo"
)

const (
	ConditionTypeExcludedResourceWarning = "ExcludedResourceWarning"
	ExcludedResourceWarningMsgPattern    = "([A-Za-z0-9.-]*)/([A-Za-z0-9.]+) ([A-Za-z0-9-_.]+)"
)

// ArgoCD is the interface for accessing Argo CD functions we need
type ArgoCD interface {
	ListApplications() ([]v1alpha1.Application, error)
	GetApplication(name string) (*v1alpha1.Application, error)
	GetAppProject(app *v1alpha1.Application) (*v1alpha1.AppProject, error)
	GetApplicationClusterServerByName(ctx context.Context, clusterName string) (string, error)
	GetResourcesFromApplicationStatus(application *v1alpha1.Application) ([]*common.ResourceInfo, error)
	GetAllMissingResources() ([]*common.ResourceInfo, error)
	GetApplicationChildManifests(ctx context.Context, application *v1alpha1.Application) ([]*common.ResourceInfo, error)
	GetTrackingMethod() (string, error)
	GetAppCluster(ctx context.Context, server string) (*v1alpha1.Cluster, error)
	GetCurrentResourceInclusions(gvr *schema.GroupVersionResource, resourceName, resourceNamespace string) (string, error)
	UpdateResourceInclusions(gvr *schema.GroupVersionResource, resourceName, resourceNamespace, resourceInclusionYaml string) error
}

// Kubernetes based client
type argocd struct {
	db                   db.ArgoDB
	kubeClient           *kube.KubeClient
	dynamicClient        dynamic.Interface
	applicationClientSet versioned.Interface
	repoServerManager    *repo.RepoServerManager
	settingsManager      *settings.SettingsManager
	applicationNamespace string
}

// NewArgoCD creates a new kube client to interact with kube api-server and
// configures access to the Argo CD repo-server.
func NewArgoCD(config *rest.Config, argocdNS string, applicationNS string, repoServerAddress string, repoServerTimeoutSeconds int, repoServerPlaintext, repoServerStrictTLS bool) (ArgoCD, error) {
	resourceTrackerConfig, err := kube.NewKubernetesClientFromConfig(context.Background(), argocdNS, config)
	if err != nil {
		return nil, fmt.Errorf("could not create K8s client: %w", err)
	}
	settingsMgr := settings.NewSettingsManager(context.Background(), resourceTrackerConfig.KubeClient.Clientset, argocdNS)
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("could not create dynamic client: %w", err)
	}
	dbInstance := db.NewDB(argocdNS, settingsMgr, resourceTrackerConfig.KubeClient.Clientset)
	repoServerManager, err := repo.NewRepoServerManager(config, argocdNS, repoServerAddress, repoServerTimeoutSeconds, repoServerPlaintext, repoServerStrictTLS)
	if err != nil {
		return nil, fmt.Errorf("could not create repo server manager: %w", err)
	}
	return &argocd{
		kubeClient:           resourceTrackerConfig.KubeClient,
		dynamicClient:        dynamicClient,
		applicationClientSet: resourceTrackerConfig.ApplicationClientSet,
		settingsManager:      settingsMgr,
		repoServerManager:    repoServerManager,
		applicationNamespace: applicationNS,
		db:                   dbInstance,
	}, nil
}

// ListApplications lists all applications across all namespaces.
func (a *argocd) ListApplications() ([]v1alpha1.Application, error) {
	ns := a.applicationNamespace
	list, err := a.applicationClientSet.ArgoprojV1alpha1().Applications(ns).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("error listing applications: %w", err)
	}
	return list.Items, nil
}

// GetApplication lists all applications across all namespaces.
func (a *argocd) GetApplication(name string) (*v1alpha1.Application, error) {
	ns := a.applicationNamespace
	application, err := a.applicationClientSet.ArgoprojV1alpha1().Applications(ns).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("error getting application %s: %w", name, err)
	}
	return application, nil
}

// GetAppProject get the associated AppProject for a given Argo CD Application.
func (a *argocd) GetAppProject(app *v1alpha1.Application) (*v1alpha1.AppProject, error) {
	// Fetch AppProject
	appProject, err := a.applicationClientSet.ArgoprojV1alpha1().AppProjects(a.kubeClient.Namespace).Get(context.Background(), app.Spec.Project, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch AppProject %s for application %s: %w", app.Spec.Project, app.Name, err)
	}
	log.Infof("Fetched AppProject: %s for application: %s", app.Spec.Project, app.Name)
	return appProject, err
}

// GetAllMissingResources returns the missing resources across all applications
func (a *argocd) GetAllMissingResources() ([]*common.ResourceInfo, error) {
	allMissingResources := make([]*common.ResourceInfo, 0)
	appList, err := a.ListApplications()
	if err != nil {
		return nil, err
	}
	for _, appObj := range appList {
		missingResources, err := getMissingResources(&appObj)
		if err != nil {
			log.Errorf("error getting missing resources from application: %v", err)
			continue
		}
		allMissingResources = append(allMissingResources, missingResources...)
	}
	return allMissingResources, nil
}

// GetTrackingMethod returns the tracking method configured for argocd
func (a *argocd) GetTrackingMethod() (string, error) {
	return a.settingsManager.GetTrackingMethod()
}

// UpdateResourceInclusions updates the resource.inclusions and resource.exclusions settings either in argocd-cm configmap or ArgoCD Custom Resource
func (a *argocd) UpdateResourceInclusions(gvr *schema.GroupVersionResource, resourceName, resourceNamespace, resourceInclusionYaml string) error {
	ctx := context.Background()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		resource, err := a.dynamicClient.Resource(*gvr).Namespace(resourceNamespace).Get(ctx, resourceName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error fetching ConfigMap: %w", err)
		}

		if err := unstructured.SetNestedField(resource.Object, resourceInclusionYaml, getResourceInclusionsHierarchy(gvr)...); err != nil {
			return fmt.Errorf("failed to set resource.inclusions value: %w", err)
		}
		if err := unstructured.SetNestedField(resource.Object, "", getResourceExclusionsHierarchy(gvr)...); err != nil {
			return fmt.Errorf("failed to set resource.exclusions value: %w", err)
		}
		// exclude all resources that are not explicitly excluded.
		unstructured.RemoveNestedField(resource.Object, "data", "resource.exclusions")

		// perform the actual update of the configmap
		_, err = a.dynamicClient.Resource(*gvr).Namespace(resourceNamespace).Update(ctx, resource, metav1.UpdateOptions{})
		if err != nil {
			log.Warningf("Retrying due to conflict: %v", err)
			return err
		}
		log.Infof("Resource inclusions updated successfully in %s/%s ConfigMap.", resourceName, resourceNamespace)
		return nil
	})
}

// GetCurrentResourceInclusions returns the resource.inclusions from argocd-cm configmap or ArgoCD Custom Resource.
func (a *argocd) GetCurrentResourceInclusions(gvr *schema.GroupVersionResource, resourceName, resourceNamespace string) (string, error) {
	argocdCM, err := a.dynamicClient.Resource(*gvr).Namespace(resourceNamespace).Get(context.Background(), resourceName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("error fetching ConfigMap: %w", err)
	}
	resourceInclusionsYaml, found, err := unstructured.NestedString(argocdCM.Object, getResourceInclusionsHierarchy(gvr)...)
	if err != nil {
		return "", err
	}
	if !found {
		log.Infof("resource inclusions not found in %s in namespace %s ", resourceName, resourceNamespace)
		return "", nil
	}
	return resourceInclusionsYaml, nil
}

// getMissingResources returns the resources that are missing to be managed via an Argo Application
func getMissingResources(obj *v1alpha1.Application) ([]*common.ResourceInfo, error) {
	conditions, err := getExcludedResourceConditions(obj.Status.Conditions)
	if err != nil {
		return nil, err
	}
	missingResources := getResourcesFromConditions(conditions)
	return missingResources, nil
}

// getExcludedResourceConditions returns the ConditionTypeExcludedResourceWarning from status.conditions of an Argo Application object
func getExcludedResourceConditions(statusConditions []v1alpha1.ApplicationCondition) ([]metav1.Condition, error) {
	resultConditions := make([]metav1.Condition, 0, len(statusConditions))
	// Marshal and Unmarshal to convert the map[string]interface{} to a Condition struct and add it only
	// if its of type ConditionTypeExcludedResourceWarning
	for _, conditionMap := range statusConditions {
		jsonBytes, err := json.Marshal(conditionMap)
		if err != nil {
			return nil, fmt.Errorf("error marshaling condition map: %w", err)
		}
		var condition metav1.Condition
		if err := json.Unmarshal(jsonBytes, &condition); err != nil {
			return nil, fmt.Errorf("error unmarshaling condition: %w", err)
		}
		if condition.Type == "ExcludedResourceWarning" {
			resultConditions = append(resultConditions, condition)
		}
	}
	return resultConditions, nil
}

// getResourcesFromConditions returns the resources that are missing to be managed reported in status.conditions
// of an Argo CD Application
func getResourcesFromConditions(conditions []metav1.Condition) []*common.ResourceInfo {
	regex := regexp.MustCompile(ExcludedResourceWarningMsgPattern)
	results := make([]*common.ResourceInfo, 0, len(conditions))
	for _, condition := range conditions {
		if condition.Type == ConditionTypeExcludedResourceWarning {
			matches := regex.FindStringSubmatch(condition.Message)
			if len(matches) > 3 {
				group := matches[1]
				kind := matches[2]
				resourceName := matches[3]
				if group == "" {
					group = "core"
				}
				results = append(results, &common.ResourceInfo{
					Group: group,
					Kind:  kind,
					Name:  resourceName,
				})
			}
		}
	}
	return results
}

func (a *argocd) GetResourcesFromApplicationStatus(application *v1alpha1.Application) ([]*common.ResourceInfo, error) {
	missingResources, err := getMissingResources(application)
	if err != nil {
		return nil, fmt.Errorf("error getting missing resources: %w", err)
	}
	results := make([]*common.ResourceInfo, 0, len(application.Status.Resources)+len(missingResources))
	for _, mr := range missingResources {
		// Convert value type to pointer type expected by the result slice.
		mrCopy := mr
		results = append(results, &common.ResourceInfo{
			Group:     mrCopy.Group,
			Kind:      mrCopy.Kind,
			Name:      mrCopy.Name,
			Namespace: mrCopy.Namespace,
		})
	}
	for _, resource := range application.Status.Resources {
		results = append(results, &common.ResourceInfo{
			Group:     resource.Group,
			Kind:      resource.Kind,
			Name:      resource.Name,
			Namespace: resource.Namespace,
		})
	}
	return results, nil
}

// getResourceInclusionsHierarchy returns the hierarchy path for getting or updating resource.inclusions for a given GVR
func getResourceInclusionsHierarchy(gvr *schema.GroupVersionResource) []string {
	if gvr.Resource == graph.ArgoCDGVR.Resource {
		return []string{"spec", "extraConfig", "resource.inclusions"}
	}
	return []string{"data", "resource.inclusions"}
}

// getResourceExclusionsHierarchy returns the hierarchy path for getting or updating resource.exclusions for a given GVR
func getResourceExclusionsHierarchy(gvr *schema.GroupVersionResource) []string {
	if gvr.Resource == graph.ArgoCDGVR.Resource {
		return []string{"spec", "extraConfig", "resource.exclusions"}
	}
	return []string{"data", "resource.exclusions"}
}

func (a *argocd) GetApplicationClusterServerByName(ctx context.Context, clusterName string) (string, error) {
	servers, err := a.db.GetClusterServersByName(ctx, clusterName)
	if err != nil {
		return "", fmt.Errorf("error getting cluster server by name %q: %w", clusterName, err)
	}
	if len(servers) > 1 {
		return "", fmt.Errorf("there are %d clusters with the same name: %v", len(servers), servers)
	}
	return servers[0], nil
}

func (a *argocd) GetAppCluster(ctx context.Context, server string) (*v1alpha1.Cluster, error) {
	cluster, err := a.db.GetCluster(ctx, server)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster: %w", err)
	}
	return cluster, nil
}

func (a *argocd) GetApplicationChildManifests(ctx context.Context, application *v1alpha1.Application) ([]*common.ResourceInfo, error) {
	appProject, err := a.GetAppProject(application)
	if err != nil {
		return nil, fmt.Errorf("error getting app project: %w", err)
	}
	childManifests, err := a.repoServerManager.GetApplicationChildManifests(ctx, application, appProject)
	if err != nil {
		return nil, fmt.Errorf("error getting child manifests: %w", err)
	}
	results := make([]*common.ResourceInfo, 0, len(childManifests))
	for _, manifest := range childManifests {
		results = append(results, &common.ResourceInfo{
			Group: manifest.GroupVersionKind().Group,
			Kind:  manifest.GroupVersionKind().Kind,
			Name:  manifest.GetName(),
		})
	}
	return results, nil
}
