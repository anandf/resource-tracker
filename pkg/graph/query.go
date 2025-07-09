package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"

	"github.com/avitaltamir/cyphernetes/pkg/core"
	"github.com/avitaltamir/cyphernetes/pkg/provider"
	"github.com/avitaltamir/cyphernetes/pkg/provider/apiserver"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
)

const (
	LabelTracking      = "$.metadata.labels.app\\.kubernetes\\.io/instance"
	AnnotationTracking = "$.metadata.annotations.argocd\\.argoproj\\.io/tracking-id"
)

type QueryServer struct {
	Executor            *core.QueryExecutor
	Provider            provider.Provider
	FieldAMatchCriteria string
	Tracker             string
	Comparison          core.ComparisonType
}

type Void struct{}
type ResourceInfoSet map[ResourceInfo]Void
type ResourceInfo struct {
	Kind       string
	APIVersion string
	Name       string
	Namespace  string
}

type Kinds map[string]Void
type GroupedResourceKinds map[string]Kinds

type ResourceInclusionEntry struct {
	APIGroups []string `json:"apiGroups,omitempty"`
	Kinds     []string `json:"kinds,omitempty"`
	Clusters  []string `json:"clusters,omitempty"`
}

const (
	ConditionTypeExcludedResourceWarning = "ConditionTypeExcludedResourceWarning"
	ExcludedResourceWarningMsgPattern    = "([a-zA-Z]*)/([a-zA-Z0-9.]+) ([a-zA-Z0-9-_.]+)"
)

var (
	ConfigMapGVR = schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "configmaps",
	}

	ArgoAppGVR = schema.GroupVersionResource{
		Group:    "argoproj.io",
		Version:  "v1alpha1",
		Resource: "applications",
	}

	CrdGVR = schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}
)

func NewQueryServer(restConfig *rest.Config, trackingMethod string) (*QueryServer, error) {
	// Create the API server provider
	p, err := apiserver.NewAPIServerProviderWithOptions(&apiserver.APIServerProviderConfig{
		Kubeconfig: restConfig,
		DryRun:     false,
		QuietMode:  true,
	})
	if err != nil {
		return nil, err
	}

	tracker := "LBL"
	fieldAMatchCriteria := LabelTracking
	comparison := core.ExactMatch
	if trackingMethod == "annotation" {
		tracker = "ANN"
		fieldAMatchCriteria = AnnotationTracking
		comparison = core.StringContains
	}

	for _, knownResourceKind := range p.(*apiserver.APIServerProvider).GetKnownResourceKinds() {
		if knownResourceKind == "projects" || knownResourceKind == "projectrequests" {
			log.Infof("skipping resource kind: %s", knownResourceKind)
			continue
		}
		core.AddRelationshipRule(core.RelationshipRule{
			KindA:        strings.ToLower(knownResourceKind),
			KindB:        "applications",
			Relationship: core.RelationshipType(strings.ToUpper(fmt.Sprintf("%s_%s_%s", "ARGOAPP_OWN", tracker, knownResourceKind))),
			MatchCriteria: []core.MatchCriterion{
				{
					FieldA:         fieldAMatchCriteria,
					FieldB:         "$.metadata.name",
					ComparisonType: comparison,
				},
			},
		})
	}
	// Create query executor with the provider
	executor := core.GetQueryExecutorInstance(p)
	if executor == nil {
		os.Exit(1)
	}
	return &QueryServer{
		Provider:            p,
		Executor:            executor,
		Tracker:             tracker,
		FieldAMatchCriteria: fieldAMatchCriteria,
		Comparison:          comparison,
	}, nil

}

func (q *QueryServer) GetApplicationChildResources(name, namespace string) (ResourceInfoSet, error) {
	return q.GetNestedChildResources(&ResourceInfo{Kind: "application", Name: name, Namespace: namespace})
}

func (q *QueryServer) GetNestedChildResources(resource *ResourceInfo) (ResourceInfoSet, error) {
	allLevelChildren := make(ResourceInfoSet)
	allLevelChildren, err := q.depthFirstTraversal(resource, allLevelChildren)
	if err != nil {
		return nil, err
	}
	return allLevelChildren, nil
}

// getChildren returns the immediate direct child of a given node by doing a graph query.
func (q *QueryServer) getChildren(parentResourceInfo *ResourceInfo) ([]*ResourceInfo, error) {
	if parentResourceInfo.Kind == "ConfigMap" || parentResourceInfo.Kind == "ServiceAccount" || parentResourceInfo.Kind == "Secret" {
		log.Infof("skipping getChildren for resource: %v", parentResourceInfo)
		return nil, nil
	}
	unambiguousKind := parentResourceInfo.Kind
	if parentResourceInfo.APIVersion == "v1" {
		unambiguousKind = fmt.Sprintf("%s.%s", "core", parentResourceInfo.Kind)
	}
	// Get the query string
	queryStr := fmt.Sprintf("MATCH (p: %s) -> (c) RETURN c.kind, c.apiVersion, c.metadata.namespace", parentResourceInfo.Kind)
	if parentResourceInfo.Name != "" {
		queryStr = fmt.Sprintf("MATCH (p: %s{name:\"%s\"}) -> (c) RETURN c.kind, c.apiVersion, c.metadata.namespace", unambiguousKind, parentResourceInfo.Name)
	}
	queryResult, err := q.executeQuery(queryStr, parentResourceInfo.Namespace)
	if err != nil {
		return nil, err
	}
	results, err := extractResourceInfo(queryResult, "c")
	if err != nil {
		return nil, err
	}
	return results, nil
}

// executeQuery executes the graph query using graph library
func (q *QueryServer) executeQuery(queryStr, namespace string) (*core.QueryResult, error) {
	// Parse the query to get an AST
	ast, err := core.ParseQuery(queryStr)
	if err != nil {
		return nil, err
	}

	// Execute the query against the Kubernetes API.
	queryResult, err := q.Executor.Execute(ast, namespace)
	if err != nil {
		return nil, err
	}
	return &queryResult, err
}

// depthFirstTraversal recursively traverses the resource tree using a DFS approach.
func (q *QueryServer) depthFirstTraversal(info *ResourceInfo, visitedNodes ResourceInfoSet) (ResourceInfoSet, error) {
	if info == nil {
		return visitedNodes, nil
	}
	log.Debugf("Visiting: %v\n", info)
	if _, ok := visitedNodes[*info]; ok {
		log.Debugf("Resource visited already: %v", info)
		return visitedNodes, nil
	}
	visitedNodes[*info] = Void{}
	// 2. Get children of the current node
	children, err := q.getChildren(info)
	if err != nil {
		log.Errorf("error getting children of resource %v : %v", info, err)
		return visitedNodes, err
	}

	// 3. Recursively call DFS for each child
	for _, child := range children {
		visitedNodes, err = q.depthFirstTraversal(child, visitedNodes)
		if err != nil {
			continue
		}
	}
	return visitedNodes, nil
}

// extractResourceInfo extracts the ResourceInfo from a given query result and variable name.
func extractResourceInfo(queryResult *core.QueryResult, variable string) ([]*ResourceInfo, error) {
	child := queryResult.Data[variable]
	if child == nil {
		return nil, nil
	}
	resourceInfoList := make([]*ResourceInfo, 0, len(child.([]interface{})))
	for _, meta := range child.([]interface{}) {
		info, ok := meta.(map[string]interface{})
		if !ok {
			continue
		}
		// Ignore namespace and node resource types, that can bring in a lot of other objects that are related to it.
		if info["kind"].(string) == "Namespace" || info["kind"].(string) == "Node" {
			continue
		}
		resourceInfo := ResourceInfo{
			Kind:       info["kind"].(string),
			APIVersion: info["apiVersion"].(string),
			Name:       info["name"].(string),
		}
		metadata, ok := info["metadata"].(map[string]interface{})
		if !ok {
			continue
		}
		namespace := metadata["namespace"]
		if namespace != nil {
			resourceInfo.Namespace = namespace.(string)
		}
		resourceInfoList = append(resourceInfoList, &resourceInfo)
	}
	return resourceInfoList, nil
}

// AddRuleForResourceKind adds the rule for a new resource kind that was added
func (q *QueryServer) AddRuleForResourceKind(resourceKind string) {
	core.AddRelationshipRule(core.RelationshipRule{
		KindA:        strings.ToLower(resourceKind),
		KindB:        "applications",
		Relationship: core.RelationshipType(strings.ToUpper(fmt.Sprintf("%s_%s_%s", "ARGOAPP_OWN", q.Tracker, resourceKind))),
		MatchCriteria: []core.MatchCriterion{
			{
				FieldA:         q.FieldAMatchCriteria,
				FieldB:         "$.metadata.name",
				ComparisonType: q.Comparison,
			},
		},
	})
}

func (k *Kinds) Equal(other *Kinds) bool {
	if len(*k) != len(*other) {
		return false
	}
	for key := range *k {
		if _, ok := (*other)[key]; !ok {
			return false
		}
	}
	return true
}

func (r *ResourceInclusionEntry) Equal(other *ResourceInclusionEntry) bool {
	if !reflect.DeepEqual(r.APIGroups, other.APIGroups) || !reflect.DeepEqual(r.Clusters, other.Clusters) || len(r.Kinds) != len(other.Kinds) {
		return false
	}
	currentKindsStr := fmt.Sprintf("%v", r.Kinds)
	for _, otherKind := range other.Kinds {
		if !strings.Contains(currentKindsStr, otherKind) {
			return false
		}
	}
	return true
}

// MergeResourceInfo merges all ResourceInfo objects according to their api groups
func MergeResourceInfo(input []ResourceInfo) GroupedResourceKinds {
	results := make(GroupedResourceKinds)
	for _, resourceInfo := range input {
		if len(resourceInfo.APIVersion) <= 0 {
			continue
		}
		apiGroup := getAPIGroup(resourceInfo.APIVersion)
		if _, found := results[apiGroup]; !found {
			results[apiGroup] = map[string]Void{
				resourceInfo.Kind: {},
			}
		} else {
			results[apiGroup][resourceInfo.Kind] = Void{}
		}
	}
	return results
}

// IsGroupedResourceKindsEqual returns true if any of the resource inclusions entries is modified, false otherwise
func IsGroupedResourceKindsEqual(previous, current GroupedResourceKinds) bool {
	if len(previous) != len(current) {
		return false
	}
	for groupName, previousKinds := range previous {
		if _, ok := current[groupName]; !ok {
			return false
		}
		currentKinds, _ := current[groupName]
		if !currentKinds.Equal(&previousKinds) {
			return false
		}
	}
	return true
}

// UpdateResourceInclusion updates the resource.inclusions and resource.exclusions settings in argocd-cm configmap
func UpdateResourceInclusion(dynamicClient dynamic.Interface, argocdNS string, resourceInclusion *GroupedResourceKinds) error {
	ctx := context.Background()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		argocdCM, err := dynamicClient.Resource(ConfigMapGVR).Namespace(argocdNS).Get(ctx, "argocd-cm", v1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error fetching ConfigMap: %v", err)
		}
		resourceInclusionString, err := GetResourceInclusionsString(resourceInclusion)
		if err != nil {
			return err
		}
		if err := unstructured.SetNestedField(argocdCM.Object, resourceInclusionString, "data", "resource.inclusions"); err != nil {
			return fmt.Errorf("failed to set resource.inclusions value: %v", err)
		}
		// exclude all resources that are not explicitly excluded.
		unstructured.RemoveNestedField(argocdCM.Object, "data", "resource.exclusions")

		// perform the actual update of the configmap
		_, err = dynamicClient.Resource(ConfigMapGVR).Namespace(argocdNS).Update(ctx, argocdCM, v1.UpdateOptions{})
		if err != nil {
			log.Warningf("Retrying due to conflict: %v", err)
			return err
		}
		log.Infof("Resource inclusions updated successfully in argocd-cm ConfigMap.")
		return nil
	})
}

// GetResourceInclusionsString returns string representation of resource inclusions
func GetResourceInclusionsString(resourceInclusion *GroupedResourceKinds) (string, error) {
	includedResources := make([]ResourceInclusionEntry, 0, len(*resourceInclusion))
	for group, kinds := range *resourceInclusion {
		includedResources = append(includedResources, ResourceInclusionEntry{
			APIGroups: []string{group},
			Kinds:     getUniqueKinds(kinds),
			Clusters:  []string{"*"},
		})
	}
	out, err := yaml.Marshal(includedResources)
	if err != nil {
		return "", err
	}
	// include resources that are managed by Argo CD.
	return string(out), nil
}

// GetCurrentGroupedKindsFromCM reads the resource.inclusions from argocd-cm config map and converts it to GroupedResourceKinds
func GetCurrentGroupedKindsFromCM(dynamicClient dynamic.Interface, argocdNS string) (GroupedResourceKinds, error) {
	argocdCM, err := dynamicClient.Resource(ConfigMapGVR).Namespace(argocdNS).Get(context.Background(), "argocd-cm", v1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("error fetching ConfigMap: %v", err)
	}
	results := make(GroupedResourceKinds)
	existingResourceInclusionsInCMStr, found, err := unstructured.NestedString(argocdCM.Object, "data", "resource.inclusions")
	if err != nil {
		return nil, err
	}
	if found {
		var existingResourceInclusionsInCM []ResourceInclusionEntry
		err = yaml.Unmarshal([]byte(existingResourceInclusionsInCMStr), &existingResourceInclusionsInCM)
		if err != nil {
			return results, nil
		}
		for _, resourceInclusion := range existingResourceInclusionsInCM {
			for _, apiGroup := range resourceInclusion.APIGroups {
				for _, kind := range resourceInclusion.Kinds {
					if results[apiGroup] == nil {
						results[apiGroup] = make(map[string]Void)
					}
					results[apiGroup][kind] = Void{}
				}
				// break after the first item in apiGroup list
				break
			}
		}
	} else {
		log.Infof("resource inclusions not found in argocd-cm in namespace %s ", argocdNS)
	}
	return results, nil
}

// ListApplicationResources returns the ResourceInfo of all Argo CD applications in the application namespace
func ListApplicationResources(dynamicClient dynamic.Interface, argocdNS, appName string) ([]ResourceInfo, error) {
	var argoAppResources []ResourceInfo
	list, err := listApplications(dynamicClient, argocdNS)
	if err != nil {
		return nil, err
	}
	for _, obj := range list.Items {
		if len(appName) > 0 {
			if appName == obj.GetName() {
				argoAppResources = append(argoAppResources, ResourceInfo{Kind: obj.GetKind(), Name: obj.GetName(), Namespace: obj.GetNamespace()})
				break
			}
		} else {
			argoAppResources = append(argoAppResources, ResourceInfo{Kind: obj.GetKind(), Name: obj.GetName(), Namespace: obj.GetNamespace()})
		}
	}
	return argoAppResources, nil
}

// GetAllMissingResources returns the missing resources across all applications
func GetAllMissingResources(dynamicClient dynamic.Interface, applicationNamespace string) ([]ResourceInfo, error) {
	allMissingResources := make([]ResourceInfo, 0)
	appList, err := listApplications(dynamicClient, applicationNamespace)
	if err != nil {
		return nil, err
	}
	for _, appObj := range appList.Items {
		missingResources, err := getMissingResources(&appObj)
		if err != nil {
			log.Errorf("error getting missing resources from application: %v", err)
			continue
		}
		allMissingResources = append(allMissingResources, missingResources...)
	}
	return allMissingResources, nil
}

// getAPIGroup returns the API group for a given API version.
func getAPIGroup(apiVersion string) string {
	if strings.Contains(apiVersion, "/") {
		return strings.Split(apiVersion, "/")[0]
	}
	return ""
}

// getUniqueKinds given a set of kinds, it returns unique set of kinds
func getUniqueKinds(kinds Kinds) []string {
	uniqueKinds := make([]string, 0)
	for kind := range kinds {
		uniqueKinds = append(uniqueKinds, kind)
	}
	return uniqueKinds
}

// listApplications returns list of Argo CD applications as unstructured objects
func listApplications(dynamicClient dynamic.Interface, namespace string) (*unstructured.UnstructuredList, error) {
	return dynamicClient.Resource(ArgoAppGVR).Namespace(namespace).List(context.Background(), v1.ListOptions{})
}

// getMissingResources returns the resources that are missing to be managed via an Argo Application
func getMissingResources(obj *unstructured.Unstructured) ([]ResourceInfo, error) {
	statusConditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	conditions, err := getExcludedResourceConditions(statusConditions)
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
func getExcludedResourceConditions(statusConditions []interface{}) ([]metav1.Condition, error) {
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

// getResourcesFromConditions returns the resources that are missing to be managed reported in status.conditions of an Argo Application
func getResourcesFromConditions(conditions []metav1.Condition) ([]ResourceInfo, error) {
	regex := regexp.MustCompile(ExcludedResourceWarningMsgPattern)
	results := make([]ResourceInfo, 0, len(conditions))
	for _, condition := range conditions {
		if condition.Type == ConditionTypeExcludedResourceWarning {
			matches := regex.FindStringSubmatch(condition.Message)
			if len(matches) > 3 {
				group := matches[1]
				kind := matches[2]
				resourceName := matches[3]
				results = append(results, ResourceInfo{
					APIVersion: group,
					Kind:       kind,
					Name:       resourceName,
				})
			}
		}
	}
	return results, nil
}
