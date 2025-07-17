package argocd

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/anandf/resource-tracker/pkg/graph"
	"github.com/anandf/resource-tracker/pkg/kube"
	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v2/pkg/client/clientset/versioned"
	"github.com/argoproj/argo-cd/v2/util/argo"
	"github.com/argoproj/argo-cd/v2/util/settings"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

const (
	ConditionTypeExcludedResourceWarning = "ConditionTypeExcludedResourceWarning"
	ExcludedResourceWarningMsgPattern    = "([a-zA-Z]*)/([a-zA-Z0-9.]+) ([a-zA-Z0-9-_.]+)"
)

// ArgoCD is the interface for accessing Argo CD functions we need
type ArgoCD interface {
	ListApplications() ([]v1alpha1.Application, error)
	GetApplication(name string) (*v1alpha1.Application, error)
	GetAppProject(app v1alpha1.Application) (*v1alpha1.AppProject, error)
	ProcessApplication(targetObjs []*unstructured.Unstructured, destinationNS string, destinationConfig *rest.Config) ([]graph.ResourceInfo, error)
	GetAllMissingResources() ([]graph.ResourceInfo, error)
	GetTrackingMethod() (string, error)
}

// Kubernetes based client
type argocd struct {
	kubeClient           *kube.KubeClient
	applicationClientSet versioned.Interface
	queryServers         map[string]*graph.QueryServer
	trackingMethod       v1alpha1.TrackingMethod
	settingsManager      *settings.SettingsManager
}

// NewArgoCD creates a new kube client to interact with kube api-server.
func NewArgoCD(config *rest.Config, argocdNS string) (ArgoCD, error) {
	resourceTrackerConfig, err := kube.NewKubernetesClientFromConfig(context.Background(), argocdNS, config)
	if err != nil {
		return nil, fmt.Errorf("could not create K8s client: %w", err)
	}
	qsMap := make(map[string]*graph.QueryServer)
	qs, err := graph.NewQueryServer(config, graph.TrackingMethodLabel, false)
	if err != nil {
		return nil, fmt.Errorf("could not create query server: %w", err)
	}
	qsMap[config.Host] = qs
	settingsMgr := settings.NewSettingsManager(context.Background(), resourceTrackerConfig.KubeClient.Clientset, argocdNS)
	return &argocd{
		kubeClient:           resourceTrackerConfig.KubeClient,
		applicationClientSet: resourceTrackerConfig.ApplicationClientSet,
		queryServers:         qsMap,
		trackingMethod:       argo.GetTrackingMethod(settingsMgr),
		settingsManager:      settingsMgr,
	}, nil
}

// ListApplications lists all applications across all namespaces.
func (a *argocd) ListApplications() ([]v1alpha1.Application, error) {
	list, err := a.applicationClientSet.ArgoprojV1alpha1().Applications(a.kubeClient.Namespace).List(context.TODO(), v1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("error listing applications: %w", err)
	}
	return list.Items, nil
}

// GetApplication lists all applications across all namespaces.
func (a *argocd) GetApplication(name string) (*v1alpha1.Application, error) {
	application, err := a.applicationClientSet.ArgoprojV1alpha1().Applications(a.kubeClient.Namespace).Get(context.TODO(), name, v1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("error getting application %s: %w", name, err)
	}
	return application, nil
}

// GetAppProject get the associated AppProject for a given Argo CD Application.
func (a *argocd) GetAppProject(app v1alpha1.Application) (*v1alpha1.AppProject, error) {
	// Fetch AppProject
	appProject, err := a.applicationClientSet.ArgoprojV1alpha1().AppProjects(a.kubeClient.Namespace).Get(context.Background(), app.Spec.Project, v1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch AppProject %s for application %s: %w", app.Spec.Project, app.Name, err)
	}
	log.Infof("Fetched AppProject: %s for application: %s", app.Spec.Project, app.Name)
	return appProject, err
}

// ProcessApplication processes a list of application managed objects and returns a list of child resources.
func (a *argocd) ProcessApplication(targetObjs []*unstructured.Unstructured, destinationNS string, destinationConfig *rest.Config) ([]graph.ResourceInfo, error) {
	var allAppChildren []graph.ResourceInfo
	for _, targetObj := range targetObjs {
		namespace := targetObj.GetNamespace()
		if len(namespace) == 0 {
			namespace = destinationNS
		}
		log.Infof("Processing target object: %s/%s of kind %s", namespace, targetObj.GetName(), targetObj.GetKind())
		qs, err := a.lookupQueryServer(destinationConfig)
		if err != nil {
			return nil, err
		}
		qs.VisitedKinds = make(map[graph.ResourceInfo]bool)
		appChildren, err := qs.GetNestedChildResources(&graph.ResourceInfo{
			Name:       targetObj.GetName(),
			Namespace:  namespace,
			Kind:       targetObj.GetKind(),
			APIVersion: targetObj.GetAPIVersion(),
		})
		if err != nil {
			return nil, err
		}
		for childRes := range appChildren {
			allAppChildren = append(allAppChildren, childRes)
		}
	}
	return allAppChildren, nil
}

// GetAllMissingResources returns the missing resources across all applications
func (a *argocd) GetAllMissingResources() ([]graph.ResourceInfo, error) {
	allMissingResources := make([]graph.ResourceInfo, 0)
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

// lookupQueryServer looks up query server for a given kubeconfig
func (a *argocd) lookupQueryServer(kubeConfig *rest.Config) (*graph.QueryServer, error) {
	if kubeConfig == nil {
		return nil, fmt.Errorf("invalid kubeConfig is nil")
	}
	if qs, ok := a.queryServers[kubeConfig.Host]; !ok {
		trackingMethod, err := a.GetTrackingMethod()
		if err != nil {
			return nil, err
		}
		newQueryServer, err := graph.NewQueryServer(kubeConfig, trackingMethod, false)
		if err != nil {
			return nil, fmt.Errorf("could not create query server: %w", err)
		}
		a.queryServers[kubeConfig.Host] = newQueryServer
		return newQueryServer, nil
	} else {
		return qs, nil
	}
}

// getMissingResources returns the resources that are missing to be managed via an Argo Application
func getMissingResources(obj *v1alpha1.Application) ([]graph.ResourceInfo, error) {
	conditions, err := getExcludedResourceConditions(obj.Status.Conditions)
	if err != nil {
		return nil, err
	}

	missingResources, err := getResourcesFromConditions(conditions)
	if err != nil {
		return nil, err
	}
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
		if condition.Type == "ConditionTypeExcludedResourceWarning" {
			resultConditions = append(resultConditions, condition)
		}
	}
	return resultConditions, nil
}

// getResourcesFromConditions returns the resources that are missing to be managed reported in status.conditions
// of an Argo CD Application
func getResourcesFromConditions(conditions []metav1.Condition) ([]graph.ResourceInfo, error) {
	regex := regexp.MustCompile(ExcludedResourceWarningMsgPattern)
	results := make([]graph.ResourceInfo, 0, len(conditions))
	for _, condition := range conditions {
		if condition.Type == ConditionTypeExcludedResourceWarning {
			matches := regex.FindStringSubmatch(condition.Message)
			if len(matches) > 3 {
				group := matches[1]
				kind := matches[2]
				resourceName := matches[3]
				results = append(results, graph.ResourceInfo{
					APIVersion: group,
					Kind:       kind,
					Name:       resourceName,
				})
			}
		}
	}
	return results, nil
}
