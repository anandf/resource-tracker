package dynamic

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/emirpasic/gods/sets/hashset"
	log "github.com/sirupsen/logrus"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextensionsinformer "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/anandf/resource-tracker/pkg/common"
)

// TODO: Rename this pkg

type ResourceMapper struct {
	DiscoveryClient        discovery.DiscoveryInterface
	InformerFactory        apiextensionsinformer.SharedInformerFactory
	ResourceList           hashset.Set
	CRDInformer            cache.SharedInformer
	DynamicClient          dynamic.Interface
	ClusterScopedResources hashset.Set
	ClusterHostname        string
}

// excludedGroupsKinds mirrors Argo CD's default resource.exclusions for
// internal / noisy resources. We skip these when building the relation cache
// to reduce API traffic and cache size.
var excludedGroupsKinds = map[string]map[string]bool{
	"": { // core
		"Endpoints": true,
	},
	"discovery.k8s.io": {
		"EndpointSlice": true,
	},
	"coordination.k8s.io": {
		"Lease": true,
	},
	"authentication.k8s.io": {
		"SelfSubjectReview": true,
		"TokenReview":       true,
	},
	"authorization.k8s.io": {
		"LocalSubjectAccessReview": true,
		"SelfSubjectAccessReview":  true,
		"SelfSubjectRulesReview":   true,
		"SubjectAccessReview":      true,
	},
	"certificates.k8s.io": {
		"CertificateSigningRequest": true,
	},
	"cert-manager.io": {
		"CertificateRequest": true,
	},
	"cilium.io": {
		"CiliumIdentity":      true,
		"CiliumEndpoint":      true,
		"CiliumEndpointSlice": true,
	},
	"kyverno.io": {
		"EphemeralReport":             true,
		"ClusterEphemeralReport":      true,
		"AdmissionReport":             true,
		"ClusterAdmissionReport":      true,
		"BackgroundScanReport":        true,
		"ClusterBackgroundScanReport": true,
		"UpdateRequest":               true,
	},
	"reports.kyverno.io": {
		"PolicyReport":        true,
		"ClusterPolicyReport": true,
	},
	"wgpolicyk8s.io": {
		"PolicyReport":        true,
		"ClusterPolicyReport": true,
	},
}

func isExcludedKind(group, kind string) bool {
	if m, ok := excludedGroupsKinds[group]; ok {
		return m[kind]
	}
	return false
}

func NewResourceMapper(destinationConfig *rest.Config) (*ResourceMapper, error) {
	destinationConfig.QPS = 50
	destinationConfig.Burst = 100
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
		DiscoveryClient:        discoveryClient,
		DynamicClient:          dynamicClient,
		InformerFactory:        informerFactory,
		CRDInformer:            crdInformer,
		ResourceList:           *hashset.New(),
		ClusterScopedResources: *hashset.New(),
		ClusterHostname:        destinationConfig.Host,
	}

	if err := rm.Init(); err != nil {
		return nil, fmt.Errorf("failed to contact cluster: %w", err)
	}

	// Set up event handlers
	_, err = crdInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: rm.addToResourceList,
		UpdateFunc: func(oldObj, newObj any) {
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
	if err != nil {
		return nil, fmt.Errorf("failed to add CRD event handler: %w", err)
	}
	return rm, nil
}

func (r *ResourceMapper) addToResourceList(obj any) {
	crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		log.Errorf("Failed to convert object to CRD")
		return
	}

	for _, version := range crd.Spec.Versions {
		if !version.Served {
			continue // Skip versions that are not served
		}

		// Check if the CRD is namespaced
		if crd.Spec.Scope != apiextensionsv1.NamespaceScoped {
			gv := fmt.Sprintf("%s/%s", crd.Spec.Group, version.Name)
			key := GetResourceKey(gv, crd.Spec.Names.Kind)
			// log.Infof("Adding cluster scoped resource: %s", key)
			r.ClusterScopedResources.Add(key)
			continue
		}

		gvr := schema.GroupVersionResource{
			Group:    crd.Spec.Group,
			Version:  version.Name,
			Resource: crd.Spec.Names.Plural, // CRD resources are named using the `plural` field
		}

		r.ResourceList.Add(gvr)
		// Add the served version of CRD to ResourceList
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
			gv, err := schema.ParseGroupVersion(resourceGroup.GroupVersion)
			if err != nil {
				continue
			}
			// Record cluster-scoped kinds for quick detection
			if !resource.Namespaced {
				groupVersion := gv.Version
				if gv.Group != "" {
					groupVersion = fmt.Sprintf("%s/%s", gv.Group, gv.Version)
				}
				key := GetResourceKey(groupVersion, resource.Kind)
				r.ClusterScopedResources.Add(key)
				// Skip adding to ResourceList, since we only scan namespaced kinds
				continue
			}
			// Skip noisy / internal resources based on Argo CD's resource.exclusions
			if isExcludedKind(gv.Group, resource.Kind) {
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

// GetResourceKey returns the key for a given resource.
func GetResourceKey(groupVersion, kind string) string {
	group := ""
	splitGroupVersion := strings.Split(groupVersion, "/")
	if len(splitGroupVersion) > 1 {
		group = splitGroupVersion[0]
	} else {
		group = "core"
	}
	return fmt.Sprintf("%s_%s", group, kind)
}

func (r *ResourceMapper) StartInformer() {
	log.Info("Starting informer for cluster ", r.ClusterHostname)
	r.InformerFactory.Start(context.Background().Done())
}

// GetResourcesRelation retrieves a mapping of parent resource kinds to their child resource kinds.
func (r *ResourceMapper) GetClusterResourcesRelation(ctx context.Context) (map[string]*hashset.Set, error) {
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
			resourceList, err := r.DynamicClient.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
				Limit:    250,
				Continue: continueToken,
			})
			if err != nil {
				if !apierrors.IsNotFound(err) {
					log.Errorf("Failed to list resource %s: %v", gvr, err)
				}
				break
			}
			// Process each resource
			for _, item := range resourceList.Items {
				// Skip excluded resources entirely
				gvk := item.GroupVersionKind()
				if isExcludedKind(gvk.Group, gvk.Kind) {
					continue
				}
				for _, ownerRef := range item.GetOwnerReferences() {
					parent := GetResourceKey(ownerRef.APIVersion, ownerRef.Kind)
					child := GetResourceKey(item.GetAPIVersion(), item.GetKind())
					// Initialize resourceRelation[parent] if not present
					if _, exists := resourceRelation[parent]; !exists {
						resourceRelation[parent] = hashset.New()
					}
					resourceRelation[parent].Add(child)
				}
				// Special case
				if item.GetKind() == "ClusterServiceVersion" {
					parent := GetResourceKey("operators.coreos.com/v1", "OperatorGroup")
					child := GetResourceKey(item.GetAPIVersion(), item.GetKind())
					if _, exists := resourceRelation[parent]; !exists {
						resourceRelation[parent] = hashset.New()
					}
					resourceRelation[parent].Add(child)
				}
				if item.GetKind() == "Secret" {
					parent := GetResourceKey("v1", "ServiceAccount")
					child := GetResourceKey(item.GetAPIVersion(), item.GetKind())
					if _, exists := resourceRelation[parent]; !exists {
						resourceRelation[parent] = hashset.New()
					}
					resourceRelation[parent].Add(child)
				}
				if item.GetKind() == "volumeClaimTemplates" {
					// add sts as parent
					parent := GetResourceKey("apps/v1", "StatefulSet")
					child := GetResourceKey(item.GetAPIVersion(), item.GetKind())
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

// GetResourceRelation builds a parent->children map of resource keys reachable
// from the provided resources, using the relation cache.
func GetResourceRelation(
	resourceRelation map[string]*hashset.Set,
	directChildren []*common.ResourceInfo,
) []*common.ResourceInfo {
	visitedKeys := make(map[string]struct{})
	for _, direct := range directChildren {
		group := direct.Group
		if group == "" {
			group = "core"
		}
		rootKey := fmt.Sprintf("%s_%s", group, direct.Kind)
		dfs(resourceRelation, rootKey, visitedKeys)
	}
	// Convert visitedKeys -> []common.ResourceInfo
	out := make([]*common.ResourceInfo, 0, len(visitedKeys))
	for key := range visitedKeys {
		parts := strings.SplitN(key, "_", 2)
		if len(parts) != 2 {
			continue
		}
		groupSeg, kind := parts[0], parts[1]
		group := groupSeg
		if groupSeg == "core" {
			group = ""
		}
		out = append(out, &common.ResourceInfo{
			Group: group,
			Kind:  kind,
			// Name/Namespace can stay empty; grouping ignores them
		})
	}
	return out
}

func dfs(rel map[string]*hashset.Set, key string, visited map[string]struct{}) {
	if _, ok := visited[key]; ok {
		return
	}
	visited[key] = struct{}{}
	childrenSet, ok := rel[key]
	if !ok {
		return
	}
	for _, v := range childrenSet.Values() {
		childKey, ok := v.(string)
		if !ok {
			continue
		}
		dfs(rel, childKey, visited)
	}
}
