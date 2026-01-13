package graph

import (
	"fmt"
	"os"
	"strings"

	"github.com/anandf/resource-tracker/pkg/common"
	"github.com/avitaltamir/cyphernetes/pkg/core"
	"github.com/avitaltamir/cyphernetes/pkg/provider"
	"github.com/avitaltamir/cyphernetes/pkg/provider/apiserver"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
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
		"ConfigMap":      true,
		"Secret":         true,
		"ServiceAccount": true,
		"Namespace":      true,
	}

	defaultIncludedResources = common.ResourceInfoSet{
		common.ResourceInfo{
			Kind:  "ConfigMap",
			Group: "",
		}: common.Void{},
		common.ResourceInfo{
			Kind:  "Secret",
			Group: "",
		}: common.Void{},
		common.ResourceInfo{
			Kind:  "ServiceAccount",
			Group: "",
		}: common.Void{},
		common.ResourceInfo{
			Kind:  "Pod",
			Group: "",
		}: common.Void{},
		common.ResourceInfo{
			Kind:  "Namespace",
			Group: "",
		}: common.Void{},
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

func NewQueryServer(restConfig *rest.Config, trackingMethod string, loadCustomRules bool) (*QueryServer, error) {
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
	if loadCustomRules {
		for _, knownResourceKind := range p.(*apiserver.APIServerProvider).GetKnownResourceKinds() {
			if blackListedKinds[knownResourceKind] || leafKinds[knownResourceKind] {
				log.Infof("skipping resource kind: %s", knownResourceKind)
				continue
			}
			relationshipTypeName := strings.ToUpper(fmt.Sprintf("%s_%s_%s", "ARGOAPP_OWN", tracker, knownResourceKind))
			if strings.Index(relationshipTypeName, ".") != -1 {
				relationshipTypeName = strings.Replace(relationshipTypeName, ".", "_", -1)
			}
			core.AddRelationshipRule(core.RelationshipRule{
				KindA:        strings.ToLower(knownResourceKind),
				KindB:        "applications.argoproj.io",
				Relationship: core.RelationshipType(relationshipTypeName),
				MatchCriteria: []core.MatchCriterion{
					{
						FieldA:         fieldAMatchCriteria,
						FieldB:         "$.metadata.name",
						ComparisonType: comparison,
					},
				},
			})
		}
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
	for resInfo := range defaultIncludedResources {
		allLevelChildren[resInfo] = common.Void{}
		q.VisitedKinds[resInfo] = true
	}
	allLevelChildren, err := q.depthFirstTraversal(resource, allLevelChildren)
	if err != nil {
		return nil, err
	}
	return allLevelChildren, nil
}

// getChildren returns the immediate direct child of a given node by doing a graph query.
func (q *QueryServer) getChildren(parentResourceInfo *common.ResourceInfo) ([]*common.ResourceInfo, error) {
	if leafKinds[parentResourceInfo.Kind] || blackListedKinds[parentResourceInfo.Kind] {
		log.Infof("skipping leaf or blacklisted resource: %v", parentResourceInfo)
		return nil, nil
	}
	visitedKindKey := common.ResourceInfo{Kind: parentResourceInfo.Kind, Group: parentResourceInfo.Group}
	if _, ok := q.VisitedKinds[visitedKindKey]; ok {
		log.Infof("skipping resource %v as kind already visited", parentResourceInfo)
		return nil, nil
	}
	unambiguousKind := parentResourceInfo.Kind
	if parentResourceInfo.Group == "" {
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

// extractResourceInfo extracts the ResourceInfo from a given query result and variable name.
func extractResourceInfo(queryResult *core.QueryResult, variable string) ([]*common.ResourceInfo, error) {
	child := queryResult.Data[variable]
	if child == nil {
		return nil, nil
	}
	resourceInfoList := make([]*common.ResourceInfo, 0, len(child.([]interface{})))
	for _, meta := range child.([]interface{}) {
		info, ok := meta.(map[string]interface{})
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
