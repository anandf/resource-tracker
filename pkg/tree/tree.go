package tree

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

var cf *genericclioptions.ConfigFlags

func GetChildResources(resourceKind, ns string) (map[types.UID]unstructured.Unstructured, error) {
	restConfig, err := cf.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	restConfig.WarningHandler = rest.NoWarnings{}
	restConfig.QPS = 1000
	restConfig.Burst = 1000
	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to construct dynamic client: %w", err)
	}
	dc, err := cf.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}

	apis, err := findAPIs(dc)
	if err != nil {
		return nil, err
	}
	klog.V(3).Info("completed querying APIs list")

	kind, name, err := figureOutKindName([]string{resourceKind})
	if err != nil {
		return nil, err
	}
	klog.V(3).Infof("parsed kind=%v name=%v", kind, name)

	var api apiResource
	if k, ok := overrideType(kind, apis); ok {
		klog.V(2).Infof("kind=%s override found: %s", kind, k.GroupVersionResource())
		api = k
	} else {
		apiResults := apis.lookup(kind)
		klog.V(5).Infof("kind matches=%v", apiResults)
		if len(apiResults) == 0 {
			return nil, fmt.Errorf("could not find api kind %q", kind)
		} else if len(apiResults) > 1 {
			names := make([]string, 0, len(apiResults))
			for _, a := range apiResults {
				names = append(names, fullAPIName(a))
			}
			return nil, fmt.Errorf("ambiguous kind %q. use one of these as the KIND disambiguate: [%s]", kind,
				strings.Join(names, ", "))
		}
		api = apiResults[0]
	}
	klog.V(2).Infof("namespace=%s", ns)

	var ri dynamic.ResourceInterface
	if api.r.Namespaced {
		ri = dyn.Resource(api.GroupVersionResource()).Namespace(ns)
	} else {
		ri = dyn.Resource(api.GroupVersionResource())
	}
	obj, err := ri.Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get %s/%s: %w", kind, name, err)
	}

	klog.V(5).Infof("target parent object: %#v", obj)

	klog.V(2).Infof("querying all api objects")
	apiObjects, err := getAllResources(dyn, apis.resources(), true)
	if err != nil {
		return nil, fmt.Errorf("error while querying api objects: %w", err)
	}
	klog.V(2).Infof("found total %d api objects", len(apiObjects))

	objs := newObjectDirectory(apiObjects)
	if len(objs.ownership[obj.GetUID()]) == 0 {
		fmt.Println("No resources are owned by this object through ownerReferences.")
		return nil, nil
	}
	return objs.items, nil
}
