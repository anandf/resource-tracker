package graph

import (
	"fmt"
	"os"
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

type queryServer struct {
	Executor *core.QueryExecutor
	Provider provider.Provider
}

type Void struct{}
type ResourceInfoSet map[ResourceInfo]Void
type ResourceInfo struct {
	Kind       string
	APIVersion string
	Name       string
	Namespace  string
}

type Kinds []string
type GroupedResourceKinds map[string]Kinds

type ResourceInclusionEntry struct {
	APIGroups []string `json:"apiGroups,omitempty"`
	Kinds     []string `json:"kinds,omitempty"`
	Clusters  []string `json:"clusters,omitempty"`
}

func NewQueryServer(restConfig *rest.Config, trackingMethod string) (*queryServer, error) {
	// Create the API server provider
	provider, err := apiserver.NewAPIServerProviderWithOptions(&apiserver.APIServerProviderConfig{
		Kubeconfig: restConfig,
		DryRun:     false,
		QuietMode:  true,
	})
	if err != nil {
		return nil, err
	}

	fieldAMatchCriteria := LabelTracking
	tracker := "LBL"
	if trackingMethod == "annotation" {
		fieldAMatchCriteria = AnnotationTracking
		tracker = "ANN"
	}

	for _, knownResourceKind := range provider.(*apiserver.APIServerProvider).GetKnownResourceKinds() {
		core.AddRelationshipRule(core.RelationshipRule{
			KindA:        strings.ToLower(knownResourceKind),
			KindB:        "applications",
			Relationship: core.RelationshipType(strings.ToUpper(fmt.Sprintf("%s_%s_%s", "ARGOAPP_OWN", tracker, knownResourceKind))),
			MatchCriteria: []core.MatchCriterion{
				{
					FieldA:         fieldAMatchCriteria,
					FieldB:         "$.metadata.name",
					ComparisonType: core.StringContains,
				},
			},
		})
	}
	// Create query executor with the provider
	executor := core.GetQueryExecutorInstance(provider)
	if executor == nil {
		os.Exit(1)
	}
	return &queryServer{
		Provider: provider,
		Executor: executor,
	}, nil

}

func (q *queryServer) GetApplicationChildResources(name, namespace string) (ResourceInfoSet, error) {
	allLevelChildren := make(ResourceInfoSet, 0)
	allLevelChildren, err := q.depthFirstTraversal(&ResourceInfo{Kind: "application", Name: name, Namespace: namespace}, allLevelChildren)
	if err != nil {
		return nil, err
	}
	return allLevelChildren, nil
}

// getChildren returns the immediate direct child of a given node by doing a graph query.
func (q *queryServer) getChildren(parentResourceInfo *ResourceInfo) ([]*ResourceInfo, error) {
	// Get the query string
	queryStr := fmt.Sprintf("MATCH (p: %s) -> (c) RETURN c.kind, c.apiVersion, c.metadata.namespace", parentResourceInfo.Kind)
	if parentResourceInfo.Name != "" {
		queryStr = fmt.Sprintf("MATCH (p: %s{name:\"%s\"}) -> (c) RETURN c.kind, c.apiVersion, c.metadata.namespace", parentResourceInfo.Kind, parentResourceInfo.Name)
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
func (q *queryServer) executeQuery(queryStr, namespace string) (*core.QueryResult, error) {
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
func (q *queryServer) depthFirstTraversal(info *ResourceInfo, visitedNodes ResourceInfoSet) (ResourceInfoSet, error) {
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
		return visitedNodes, nil
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
