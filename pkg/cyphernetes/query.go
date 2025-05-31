package cyphernetes

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/avitaltamir/cyphernetes/pkg/core"
	"github.com/avitaltamir/cyphernetes/pkg/provider"
	"github.com/avitaltamir/cyphernetes/pkg/provider/apiserver"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type queryServer struct {
	Executor *core.QueryExecutor
	Provider provider.Provider
}

type void struct{}
type ResourceInfoSet map[ResourceInfo]void
type ResourceInfo struct {
	kind       string
	apiVersion string
	name       string
	namespace  string
}

type Kinds []string
type ResourceInclusionEntry map[string]Kinds

func NewQueryServer() (*queryServer, error) {
	var restConfig *rest.Config
	// First try in-cluster config
	restConfig, err := rest.InClusterConfig()
	if err != nil && !errors.Is(err, rest.ErrNotInCluster) {
		return nil, fmt.Errorf("failed to create config: %v", err)
	}

	// If not running in-cluster, try loading from KUBECONFIG env or $HOME/.kube/config file
	if restConfig == nil {
		// Fall back to kubeconfig
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		configOverrides := &clientcmd.ConfigOverrides{}
		kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
		restConfig, err = kubeConfig.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to create config: %v", err)
		}
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %v", err)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %v", err)
	}

	// Create the API server provider
	provider, err := apiserver.NewAPIServerProviderWithOptions(&apiserver.APIServerProviderConfig{
		Clientset:     client,
		DynamicClient: dynamicClient,
		DryRun:        false,
		QuietMode:     true,
	})
	if err != nil {
		return nil, err
	}
	core.LogLevel = "debug"
	// Create query executor with the provider
	executor := core.GetQueryExecutorInstance(provider)
	if executor == nil {
		os.Exit(1)
	}
	if err := core.InitResourceSpecs(executor.Provider()); err != nil {
		fmt.Printf("Error initializing resource specs: %v\n", err)
	}

	return &queryServer{
		Provider: provider,
		Executor: executor,
	}, nil

}

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

func (q *queryServer) extractResourceInfo(queryResult *core.QueryResult, variable string) ([]ResourceInfo, error) {
	child := queryResult.Data[variable]
	if child == nil {
		return []ResourceInfo{}, nil
	}
	resourceInfoList := make([]ResourceInfo, 0, len(child.([]interface{})))
	for _, meta := range child.([]interface{}) {
		info, ok := meta.(map[string]interface{})
		if !ok {
			continue
		}
		if info["kind"].(string) == "Namespace" {
			continue
		}
		resourceInfo := ResourceInfo{
			kind:       info["kind"].(string),
			apiVersion: info["apiVersion"].(string),
			name:       info["name"].(string),
		}
		metadata, ok := info["metadata"].(map[string]interface{})
		if !ok {
			continue
		}
		namespace := metadata["namespace"]
		if namespace != nil {
			resourceInfo.namespace = namespace.(string)
		}
		resourceInfoList = append(resourceInfoList, resourceInfo)
	}
	return resourceInfoList, nil
}

func (q *queryServer) GetApplicationChildResources() (ResourceInfoSet, error) {
	allLevelChildren := make(ResourceInfoSet, 0)
	directChildren, err := q.getChildren("application", "", "")
	if err != nil {
		return allLevelChildren, err
	}
	for _, directChild := range directChildren {
		fmt.Println("getting children for resource: ", directChild)
		allLevelChildren[directChild] = void{}
		if directChild.kind == "Namespace" {
			continue
		}
		level2Children, err := q.getChildren(strings.ToLower(directChild.kind), directChild.name, directChild.namespace)
		if err != nil {
			return allLevelChildren, err
		}
		for _, level2Child := range level2Children {
			allLevelChildren[level2Child] = void{}
			if level2Child.kind == "Namespace" {
				continue
			}
			level3Children, err := q.getChildren(strings.ToLower(level2Child.kind), level2Child.name, level2Child.namespace)
			if err != nil {
				return allLevelChildren, err
			}
			for _, level3Child := range level3Children {
				allLevelChildren[level3Child] = void{}
				if level3Child.kind == "Namespace" {
					continue
				}
				level4Children, err := q.getChildren(strings.ToLower(level3Child.kind), level3Child.name, level3Child.namespace)
				if err != nil {
					return allLevelChildren, err
				}
				for _, level4Child := range level4Children {
					allLevelChildren[level4Child] = void{}
				}
			}
		}
	}
	resourceInclusion := mergeResourceInfo(allLevelChildren)
	for k, v := range resourceInclusion {
		fmt.Printf("key: %s, value: %v\n", k, v)
	}
	return allLevelChildren, nil
}

func (q *queryServer) getChildren(parentKind, parentName, parentNamespace string) ([]ResourceInfo, error) {
	// Get the query string
	queryStr := fmt.Sprintf("MATCH (p: %s) -> (c) RETURN c.kind, c.apiVersion, c.metadata.namespace", parentKind)
	if parentName != "" {
		queryStr = fmt.Sprintf("MATCH (p: %s{name:\"%s\"}) -> (c) RETURN c.kind, c.apiVersion, c.metadata.namespace", parentKind, parentName)
	}
	queryResult, err := q.executeQuery(queryStr, parentNamespace)
	if err != nil {
		return nil, err
	}
	results, err := q.extractResourceInfo(queryResult, "c")
	if err != nil {
		return nil, err
	}
	return results, nil
}

func mergeResourceInfo(input map[ResourceInfo]void) ResourceInclusionEntry {
	results := make(ResourceInclusionEntry, 0)

	for resourceInfo, _ := range input {
		apiGroup := getAPIGroup(resourceInfo.apiVersion)
		kinds, ok := results[apiGroup]
		if !ok {
			results[apiGroup] = []string{resourceInfo.kind}
		} else {
			results[apiGroup] = append(kinds, resourceInfo.kind)
		}
	}
	return results
}

func getAPIGroup(apiVersion string) string {
	if strings.Contains(apiVersion, "/") {
		return strings.Split(apiVersion, "/")[0]
	}
	return ""
}
