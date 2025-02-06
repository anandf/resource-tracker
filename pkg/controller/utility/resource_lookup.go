package utility

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// GetResourceRelation constructs a parent-child relationship map for the given resources.
func GetResourceRelation(configMap map[string]string, resources []*unstructured.Unstructured) map[string][]string {
	// Initialize the parent-child map
	parentChildMap := make(map[string][]string)

	// Helper function to get the key for a resource
	getResourceKey := func(resource *unstructured.Unstructured) string {
		group := ""
		groupVersion := strings.Split(resource.GetAPIVersion(), "/")
		if len(groupVersion) > 1 {
			group = groupVersion[0]
		} else {
			group = "core"
		}
		return fmt.Sprintf("%s_%s", group, resource.GetKind())
	}
	// Iterate over the resources and build the parent-child map
	for _, resource := range resources {
		resourceKey := getResourceKey(resource)
		buildResourceTree(configMap, resourceKey, parentChildMap)
	}

	return parentChildMap
}

func buildResourceTree(configMap map[string]string, resourceKey string, parentChildMap map[string][]string) {
	// Get the parent resource
	childResourceKeys, ok := configMap[resourceKey]
	if !ok {
		return
	}
	// Recursively build the tree
	for _, childResourceKey := range strings.Split(childResourceKeys, ",") {
		// Add the parent-child relationship to the map
		parentChildMap[resourceKey] = append(parentChildMap[resourceKey], childResourceKey)
		buildResourceTree(configMap, childResourceKey, parentChildMap)
	}

}
