package graph

import (
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/avitaltamir/cyphernetes/pkg/core"
	"github.com/avitaltamir/cyphernetes/pkg/provider"
	"github.com/avitaltamir/cyphernetes/pkg/provider/apiserver"
	log "github.com/sirupsen/logrus"

	"k8s.io/client-go/rest"
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
	allLevelChildren := make(ResourceInfoSet)
	allLevelChildren, err := q.depthFirstTraversal(&ResourceInfo{Kind: "application", Name: name, Namespace: namespace}, allLevelChildren)
	if err != nil {
		return nil, err
	}
	return allLevelChildren, nil
}

// getChildren returns the immediate direct child of a given node by doing a graph query.
func (q *QueryServer) getChildren(parentResourceInfo *ResourceInfo) ([]*ResourceInfo, error) {
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
		log.Error("error getting children: %v", err)
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
