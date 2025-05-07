package resourcegraph

import (
	"context"
	"fmt"
	"reflect"

	"github.com/anandf/resource-tracker/pkg/kube"
	"github.com/emirpasic/gods/sets/hashset"
	log "github.com/sirupsen/logrus"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextensionsinformer "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
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
		log.Errorf("Error loading native resources: %v", err)
	}

	// Set up event handlers
	crdInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: rm.addToResourceList,
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldCRD, ok1 := oldObj.(*apiextensionsv1.CustomResourceDefinition)
			newCRD, ok2 := newObj.(*apiextensionsv1.CustomResourceDefinition)
			if !ok1 || !ok2 {
				log.Errorf("Failed to convert objects to CRD")
				return
			}
			// Compare the spec fields before reprocessing
			if reflect.DeepEqual(oldCRD.Spec, newCRD.Spec) {
				log.Debugf("CRD %s update ignored as there is no spec change", newCRD.Name)
				return
			}
			rm.addToResourceList(newObj)
		},
	})
	return rm, nil
}

func (r *ResourceMapper) addToResourceList(obj interface{}) {
	crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		log.Errorf("Failed to convert object to CRD")
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
		// Log whether we're updating or adding a new CRD version
		if r.ResourceList.Contains(gvr) {
			log.Debugf("Updating existing CRD version: %s/%s (%s)", gvr.Group, gvr.Version, gvr.Resource)
		} else {
			log.Infof("Adding new CRD version: %s/%s (%s)", gvr.Group, gvr.Version, gvr.Resource)
		}
		// Add the served version of CRD to ResourceList
		r.ResourceList.Add(gvr)
	}
}

func (r *ResourceMapper) Init() error {
	// Get API resource list
	resourceList, err := r.DiscoveryClient.ServerPreferredResources()
	if err != nil {
		if len(resourceList) == 0 {
			return err
		}
		log.Errorf("Partial success when performing preferred resource discovery: %v", err)
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
	log.Info("Starting informer for cluster")
	r.InformerFactory.Start(context.Background().Done())
}

// GetResourcesRelation retrieves a mapping of parent resource kinds to their child resource kinds.
func (r *ResourceMapper) GetResourcesRelation(ctx context.Context) (map[string]*hashset.Set, error) {
	resourceRelation := make(map[string]*hashset.Set)
	// Iterate over all API resources
	for _, value := range r.ResourceList.Values() {
		gvr, ok := value.(schema.GroupVersionResource)
		if !ok {
			log.Errorf("Failed to assert type for gvr: %v", value)
			continue
		}
		var continueToken string
		for {
			resourceList, err := r.DynamicClient.Resource(gvr).Namespace(v1.NamespaceAll).List(ctx, v1.ListOptions{
				Limit:    250,
				Continue: continueToken,
			})
			if err != nil {
				log.Errorf("Failed to list resources for %s: %v", gvr.Resource, err)
				break
			}
			// Process each resource
			for _, item := range resourceList.Items {
				for _, ownerRef := range item.GetOwnerReferences() {
					parent := kube.GetResourceKey(ownerRef.APIVersion, ownerRef.Kind)
					child := kube.GetResourceKey(item.GetAPIVersion(), item.GetKind())
					// Initialize resourceRelation[parent] if not present
					if _, exists := resourceRelation[parent]; !exists {
						resourceRelation[parent] = hashset.New()
					}
					resourceRelation[parent].Add(child)
				}
				// Special case
				if item.GetKind() == "ClusterServiceVersion" {
					parent := kube.GetResourceKey("operators.coreos.com/v1", "OperatorGroup")
					child := kube.GetResourceKey(item.GetAPIVersion(), item.GetKind())
					if _, exists := resourceRelation[parent]; !exists {
						resourceRelation[parent] = hashset.New()
					}
					resourceRelation[parent].Add(child)
				}
				if item.GetKind() == "Secret" {
					parent := kube.GetResourceKey("v1", "ServiceAccount")
					child := kube.GetResourceKey(item.GetAPIVersion(), item.GetKind())
					if _, exists := resourceRelation[parent]; !exists {
						resourceRelation[parent] = hashset.New()
					}
					resourceRelation[parent].Add(child)
				}
				if item.GetKind() == "volumeClaimTemplates" {
					// add sts as parent
					parent := kube.GetResourceKey("apps/v1", "StatefulSet")
					child := kube.GetResourceKey(item.GetAPIVersion(), item.GetKind())
					if _, exists := resourceRelation[parent]; !exists {
						resourceRelation[parent] = hashset.New()
					}
					resourceRelation[parent].Add(child)
				}
			}
			// Check if there are more results to fetch
			continueToken = resourceList.GetContinue()
			if continueToken == "" {
				break
			}
		}
	}

	return resourceRelation, nil
}
