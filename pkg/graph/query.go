package graph

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/avitaltamir/cyphernetes/pkg/core"
	"github.com/avitaltamir/cyphernetes/pkg/provider"
	"github.com/avitaltamir/cyphernetes/pkg/provider/apiserver"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	aggregator "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"

	"github.com/anandf/resource-tracker/pkg/common"
)

const (
	LabelTrackingCriteria      = "$.metadata.labels.app\\.kubernetes\\.io/instance"
	AnnotationTrackingCriteria = "$.metadata.annotations.argocd\\.argoproj\\.io/tracking-id"
	TrackingMethodLabel        = "label"
	TrackingMethodAnnotation   = "annotation"
)

var (
	ArgoAppGVR = schema.GroupVersionResource{
		Group:    "argoproj.io",
		Version:  "v1alpha1",
		Resource: "applications",
	}
	ArgoCDGVR = schema.GroupVersionResource{
		Group:    "argoproj.io",
		Version:  "v1beta1",
		Resource: "argocds",
	}
	ConfigMapGVR = schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "configmaps",
	}

	SecretGVR = schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "secrets",
	}

	CrdGVR = schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}
)

var (
	blackListedKinds = map[string]bool{
		"projects":        true,
		"projectRequests": true,
		"configmaps":      true,
		"secrets":         true,
		"serviceaccounts": true,
		"pods":            true,
		"nodes":           true,
		"apiservices":     true,
		"namespaces":      true,
	}

	leafKinds = map[string]bool{
		"Role":                       true,
		"RoleBinding":                true,
		"ClusterRole":                true,
		"ClusterRoleBinding":         true,
		"ConfigMap":                  true,
		"Secret":                     true,
		"ServiceAccount":             true,
		"Namespace":                  true,
		"PersistentVolume":           true,
		"PersistentVolumeClaim":      true,
		"Endpoints":                  true,
		"EndpointSlice":              true,
		"NetworkPolicy":              true,
		"Ingress":                    true,
		"Route":                      true,
		"SecurityContextConstraints": true,
	}
)

type QueryServer struct {
	Executor            *core.QueryExecutor
	Provider            provider.Provider
	FieldAMatchCriteria string
	Tracker             string
	Comparison          core.ComparisonType
	VisitedKinds        map[common.ResourceInfo]bool
}

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
	fieldAMatchCriteria := LabelTrackingCriteria
	comparison := core.ExactMatch
	if trackingMethod == TrackingMethodAnnotation {
		tracker = "ANN"
		fieldAMatchCriteria = AnnotationTrackingCriteria
		comparison = core.StringContains
	}
	if isOpenShiftCluster(restConfig) {
		log.Info("OpenShift cluster detected, adding OpenShift specific rules")
		addOpenShiftSpecificRules()
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
		VisitedKinds:        make(map[common.ResourceInfo]bool),
	}, nil
}

func (q *QueryServer) GetApplicationChildResources(name, namespace string) (common.ResourceInfoSet, error) {
	return q.GetNestedChildResources(&common.ResourceInfo{
		Kind:      "applications.argoproj.io",
		Group:     "argoproj.io",
		Name:      name,
		Namespace: namespace,
	})
}

func (q *QueryServer) GetNestedChildResources(resource *common.ResourceInfo) (common.ResourceInfoSet, error) {
	allLevelChildren := make(common.ResourceInfoSet)
	allLevelChildren, err := q.depthFirstTraversal(resource, allLevelChildren)
	if err != nil {
		return nil, err
	}
	return allLevelChildren, nil
}

// getChildren returns the immediate direct child of a given node by doing a graph query.
func (q *QueryServer) getChildren(parentResourceInfo *common.ResourceInfo) ([]*common.ResourceInfo, error) {
	if leafKinds[parentResourceInfo.Kind] || blackListedKinds[parentResourceInfo.Kind] {
		log.Debugf("skipping leaf or blacklisted resource: %v", parentResourceInfo)
		return nil, nil
	}
	visitedKindKey := common.ResourceInfo{Kind: parentResourceInfo.Kind, Group: parentResourceInfo.Group}
	if _, ok := q.VisitedKinds[visitedKindKey]; ok {
		log.Debugf("skipping resource %v as kind already visited", parentResourceInfo)
		return nil, nil
	}
	var unambiguousKind string
	if parentResourceInfo.Group == "" {
		unambiguousKind = fmt.Sprintf("%s.%s", "core", parentResourceInfo.Kind)
	} else {
		pluralKind := strings.ToLower(parentResourceInfo.Kind) + "s"
		unambiguousKind = fmt.Sprintf("%s.%s", pluralKind, parentResourceInfo.Group)
	}
	// Get the query string
	queryStr := fmt.Sprintf("MATCH (p: %s) -> (c) RETURN c.kind, c.apiVersion, c.metadata.namespace", unambiguousKind)
	if parentResourceInfo.Name != "" {
		queryStr = fmt.Sprintf("MATCH (p: %s{name:%q}) -> (c) RETURN c.kind, c.apiVersion, c.metadata.namespace", unambiguousKind, parentResourceInfo.Name)
	}
	queryResult, err := q.executeQuery(queryStr, parentResourceInfo.Namespace)
	if err != nil {
		return nil, err
	}
	results := extractResourceInfo(queryResult, "c")
	q.VisitedKinds[visitedKindKey] = true
	return results, nil
}

// executeQuery executes the graph query using graph library
func (q *QueryServer) executeQuery(queryStr, namespace string) (*core.QueryResult, error) {
	// Parse the query to get an AST
	ast, err := core.ParseQuery(queryStr)
	if err != nil {
		return nil, err
	}
	// If namespace is empty, set all namespaces to true
	if namespace == "" {
		core.AllNamespaces = true
	} else {
		core.AllNamespaces = false
	}

	// Execute the query against the Kubernetes API.
	queryResult, err := q.Executor.Execute(ast, namespace)
	if err != nil {
		return nil, err
	}
	return &queryResult, err
}

// depthFirstTraversal recursively traverses the resource tree using a DFS approach.
func (q *QueryServer) depthFirstTraversal(info *common.ResourceInfo, visitedNodes common.ResourceInfoSet) (common.ResourceInfoSet, error) {
	if info == nil {
		return visitedNodes, nil
	}
	log.Debugf("Visiting: %v\n", info)
	if _, ok := visitedNodes[*info]; ok {
		log.Debugf("Resource visited already: %v", info)
		return visitedNodes, nil
	}
	visitedNodes[*info] = common.Void{}
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
func extractResourceInfo(queryResult *core.QueryResult, variable string) []*common.ResourceInfo {
	child := queryResult.Data[variable]
	if child == nil {
		return nil
	}
	resourceInfoList := make([]*common.ResourceInfo, 0, len(child.([]any)))
	for _, meta := range child.([]any) {
		info, ok := meta.(map[string]any)
		if !ok {
			continue
		}
		// Ignore namespace and node resource types, that can bring in a lot of other objects that are related to it.
		if info["kind"] == nil || info["kind"].(string) == "Namespace" || info["kind"].(string) == "Node" || info["kind"].(string) == "APIService" {
			log.Infof("ignoring resource of kind: %v", info["kind"])
			continue
		}
		apiVersion, _ := info["apiVersion"].(string)
		group := ""
		if apiVersion != "" {
			parts := strings.Split(apiVersion, "/")
			if len(parts) == 2 {
				group = parts[0]
			}
		}
		resourceInfo := common.ResourceInfo{
			Kind:  info["kind"].(string),
			Group: group,
			Name:  info["name"].(string),
		}
		metadata, ok := info["metadata"].(map[string]any)
		if !ok {
			continue
		}
		namespace := metadata["namespace"]
		if namespace != nil {
			resourceInfo.Namespace = namespace.(string)
		}
		resourceInfoList = append(resourceInfoList, &resourceInfo)
	}
	return resourceInfoList
}

// addOpenShiftSpecificRules adds rules that are specific to OpenShift CustomResources
func addOpenShiftSpecificRules() {
	core.AddRelationshipRule(core.RelationshipRule{
		KindA:        "hostfirmwaresettings",
		KindB:        "baremetalhosts",
		Relationship: "BAREMETALHOSTS_OWN_HOSTFIRMWARE_SETTINGS",
		MatchCriteria: []core.MatchCriterion{
			{
				FieldA:         "$.metadata.ownerReferences[].name",
				FieldB:         "$.metadata.name",
				ComparisonType: core.ExactMatch,
			},
		},
	})
}

func isOpenShiftCluster(restConfig *rest.Config) bool {
	aggregatorClient, err := aggregator.NewForConfig(restConfig)
	if err != nil {
		return false
	}
	gv := schema.GroupVersion{
		Group:   "config.openshift.io",
		Version: "v1",
	}
	if err = discovery.ServerSupportsVersion(aggregatorClient, gv); err != nil {
		// check if the API is registered
		_, err = aggregatorClient.ApiregistrationV1().APIServices().
			Get(context.TODO(), fmt.Sprintf("%s.%s", gv.Version, gv.Group), metav1.GetOptions{})
		return err == nil
	}
	return true
}
