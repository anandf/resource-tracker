package kube

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// GetResourceRelation builds a parent-child relationship map for the given resources based on the provided configMap.
func GetResourceRelation(configMap map[string]string, resources []*unstructured.Unstructured) map[string][]string {
	// Initialize the parent-child map
	parentChildMap := make(map[string][]string)
	// Iterate over the resources and build the parent-child map
	for _, resource := range resources {
		resourceKey := GetResourceKey(resource.GetAPIVersion(), resource.GetKind())
		visited := make(map[string]struct{})
		buildResourceTree(configMap, resourceKey, parentChildMap, visited)
		// Ensure the resource is added even if it has no children
		if _, exists := parentChildMap[resourceKey]; !exists {
			parentChildMap[resourceKey] = []string{} // Add it with an empty child list
		}
	}
	return parentChildMap
}

func buildResourceTree(configMap map[string]string, resourceKey string, parentChildMap map[string][]string, visited map[string]struct{}) {
	// Check if the resource has already been visited to avoid circular dependency
	if _, found := visited[resourceKey]; found {
		return
	}
	visited[resourceKey] = struct{}{}
	// Get the parent resource
	childResourceKeys, ok := configMap[resourceKey]
	if !ok {
		return
	}
	// Recursively build the tree
	for _, childResourceKey := range strings.Split(childResourceKeys, ",") {
		// Add the parent-child relationship to the map
		parentChildMap[resourceKey] = append(parentChildMap[resourceKey], childResourceKey)
		buildResourceTree(configMap, childResourceKey, parentChildMap, visited)
	}
}

// GetResourceKey returns the key for a given resource.
func GetResourceKey(groupVersion string, kind string) string {
	group := ""
	splitGroupVersion := strings.Split(groupVersion, "/")
	if len(splitGroupVersion) > 1 {
		group = splitGroupVersion[0]
	} else {
		group = "core"
	}
	return fmt.Sprintf("%s_%s", group, kind)
}
