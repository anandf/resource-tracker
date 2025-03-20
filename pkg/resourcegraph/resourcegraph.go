package resourcegraph

import (
	"context"
	"fmt"
	"slices"

	"github.com/anandf/resource-tracker/pkg/kube"
	"github.com/emirpasic/gods/sets/hashset"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextensionsinformer "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type ResourceMapper struct {
	DiscoveryClient discovery.DiscoveryInterface
	InformerFactory apiextensionsinformer.SharedInformerFactory
	ResourceList    hashset.Set
	CRDInformer     cache.SharedInformer
	DynamicClient   dynamic.Interface
}

func NewResourceMapper(destinationConfig *rest.Config) (*ResourceMapper, error) {
	apiextensionsClient, err := apiextensionsclient.NewForConfig(destinationConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create apiextensions client: %w", err)
	}

	// Create informer factory
	informerFactory := apiextensionsinformer.NewSharedInformerFactory(apiextensionsClient, 0)

	// Get CRD informer
	crdInformer := informerFactory.Apiextensions().V1().CustomResourceDefinitions().Informer()

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(destinationConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create discovery client: %w", err)
	}
	// Create dynamic client
	dynamicClient, err := dynamic.NewForConfig(destinationConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	rm := &ResourceMapper{
		DiscoveryClient: discoveryClient,
		DynamicClient:   dynamicClient,
		InformerFactory: informerFactory,
		CRDInformer:     crdInformer,
		ResourceList:    *hashset.New(),
	}

	if err := rm.Init(); err != nil {
		klog.Errorf("Error loading native resources: %v", err)
	}

	// Set up event handlers
	crdInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: rm.addToResourceList,
		UpdateFunc: func(oldObj, newObj interface{}) {
			rm.addToResourceList(newObj)
		},
	})
	return rm, nil
}

func (r *ResourceMapper) addToResourceList(obj interface{}) {
	crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		klog.Errorf("Failed to convert object to CRD")
		return
	}

	for _, version := range crd.Spec.Versions {
		if !version.Served {
			continue // Skip versions that are not served
		}

		gvr := schema.GroupVersionResource{
			Group:    crd.Spec.Group,
			Version:  version.Name,
			Resource: crd.Spec.Names.Plural, // CRD resources are named using the `plural` field
		}

		// Add the served version of CRD to ResourceList
		r.ResourceList.Add(gvr)
		klog.Infof("New CRD version added: %s/%s (%s)", gvr.Group, gvr.Version, gvr.Resource)
	}
}

func (r *ResourceMapper) Init() error {
	// Get API resource list
	resourceList, err := r.DiscoveryClient.ServerPreferredResources()
	if err != nil {
		if discovery.IsGroupDiscoveryFailedError(err) {
			klog.Warningf("Skipping stale API groups: %v", err)
		} else {
			klog.Errorf("Failed to get API resource list: %v", err)
			return fmt.Errorf("failed to get API resource list: %w", err)
		}
	}
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
			r.ResourceList.Add(gvr)
		}

	}
	return nil
}

func (r *ResourceMapper) StartInformer() {
	klog.Infof("Starting informer for cluster")
	r.InformerFactory.Start(context.Background().Done())
}

// GetResourcesRelationFilter retrieves a mapping of parent resource kinds to their child resource kinds.
func (r *ResourceMapper) GetResourcesRelationFilter(ctx context.Context) (map[string][]string, error) {
	childMap := make(map[string][]unstructured.Unstructured)
	directChildren := make(map[string]struct{})
	resourceRelation := make(map[string][]string)
	// Iterate over all API resources
	for _, value := range r.ResourceList.Values() {
		// Get all resources in the namespace
		gvr, ok := value.(schema.GroupVersionResource)
		if !ok {
			klog.Errorf("Failed to assert type for gvr: %v", gvr)
			continue
		}
		resourceList, err := r.DynamicClient.Resource(gvr).Namespace(v1.NamespaceAll).List(ctx, v1.ListOptions{})
		if err != nil {
			klog.Errorf("Failed to list resources for %s: %v", gvr.Resource, err)
			continue
		}
		// Check owner references
		for _, item := range resourceList.Items {
			if isDirectChild(item) {
				directChildren[string(item.GetUID())] = struct{}{}
			}
			for _, ownerRef := range item.GetOwnerReferences() {
				parentUID := string(ownerRef.UID)
				childMap[parentUID] = append(childMap[parentUID], item)
			}
		}

	}
	for parentUID, items := range childMap {
		if _, idExists := directChildren[parentUID]; idExists {
			for _, item := range items {
				for _, ownerRef := range item.GetOwnerReferences() {
					child := kube.GetResourceKey(item.GetAPIVersion(), item.GetKind())
					parent := kube.GetResourceKey(ownerRef.APIVersion, ownerRef.Kind)
					// Avoid duplicates
					if !slices.Contains(resourceRelation[parent], child) {
						resourceRelation[parent] = append(resourceRelation[parent], child)
					}
				}
			}
		}
	}

	// Example return format:
	// map[apps_DaemonSet:[core_Pod apps_ControllerRevision] apps_Deployment:[apps_ReplicaSet] apps_ReplicaSet:[core_Pod] apps_StatefulSet:[core_Pod apps_ControllerRevision] core_Node:[core_Pod coordination.k8s.io_Lease] core_Service:[discovery.k8s.io_EndpointSlice]]
	return resourceRelation, nil
}

// Helper to check if an object is a direct child of an application
func isDirectChild(obj unstructured.Unstructured) bool {
	annotations := obj.GetAnnotations()
	_, idExists := annotations["argocd.argoproj.io/tracking-id"]
	return idExists
}
