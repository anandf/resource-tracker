package resourcegraph

import (
	"context"
	"fmt"

	"slices"

	"github.com/anandf/resource-tracker/pkg/kube"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

// GetResourcesRelation retrieves a mapping of parent resource kinds to their child resource kinds.
func GetResourcesRelation(ctx context.Context, config *rest.Config) (map[string][]string, error) {
	// Create discovery client
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create discovery client: %w", err)
	}
	// Get API resource list
	resourceList, err := discoveryClient.ServerPreferredResources()
	if err != nil {
		if discovery.IsGroupDiscoveryFailedError(err) {
			klog.Warningf("Skipping stale API groups: %v", err)
		} else {
			klog.Errorf("Failed to get API resource list: %v", err)
			return nil, err
		}
	}
	// Create dynamic client
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}
	childMap := make(map[string][]string)
	// Iterate over all API resources
	for _, resourceGroup := range resourceList {
		for _, resource := range resourceGroup.APIResources {
			if !resource.Namespaced { // Skip non-namespaced resources
				continue
			}
			gv, err := schema.ParseGroupVersion(resourceGroup.GroupVersion)
			if err != nil {
				continue
			}
			gvr := schema.GroupVersionResource{
				Group:    gv.Group,
				Version:  gv.Version,
				Resource: resource.Name,
			}
			// Get all resources in the namespace
			resourceList, err := dynamicClient.Resource(gvr).Namespace(v1.NamespaceAll).List(ctx, v1.ListOptions{})
			if err != nil {
				klog.Errorf("Failed to list resources for %s: %v", resource.Name, err)
				continue
			}
			// NEED INPROVEMNT TO RETURN ONLY ARGOCD RELATED RESOURCES
			// Check owner references
			for _, item := range resourceList.Items {
				for _, ownerRef := range item.GetOwnerReferences() {
					child := kube.GetResourceKey(item.GetAPIVersion(), item.GetKind())
					parent := kube.GetResourceKey(ownerRef.APIVersion, ownerRef.Kind)
					// Avoid duplicates
					if !slices.Contains(childMap[parent], child) {
						childMap[parent] = append(childMap[parent], child)
					}
				}
			}
		}
	}

	// Example return format:
	// map[apps_DaemonSet:[core_Pod apps_ControllerRevision] apps_Deployment:[apps_ReplicaSet] apps_ReplicaSet:[core_Pod] apps_StatefulSet:[core_Pod apps_ControllerRevision] core_Node:[core_Pod coordination.k8s.io_Lease] core_Service:[discovery.k8s.io_EndpointSlice]]
	return childMap, nil
}
