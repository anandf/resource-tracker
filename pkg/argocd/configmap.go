package argocd

import (
	"context"
	"fmt"
	"strings"

	"github.com/anandf/resource-tracker/pkg/resourcegraph"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

const (
	ARGOCD_CM                = "argocd-cm"
	RESOURCE_RELATION_LOOKUP = "resource-relation-lookup"
)

type ResourceInclusion struct {
	APIGroups []string `yaml:"apiGroups"`
	Kinds     []string `yaml:"kinds"`
	Clusters  []string `yaml:"clusters"`
}

func UpdateResourceInclusion(resourceTree map[string][]string, k8sclient kubernetes.Interface, namespace string) error {
	configMap, err := k8sclient.CoreV1().ConfigMaps(namespace).Get(context.Background(), ARGOCD_CM, v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error fetching ConfigMap: %v", err)
	}
	groupVersion := groupResourcesByAPIGroup(resourceTree)
	var existingResourceInclusions []ResourceInclusion
	if existingYamlData, exists := configMap.Data["resource.inclusions"]; exists {
		if err := yaml.Unmarshal([]byte(existingYamlData), &existingResourceInclusions); err != nil {
			return fmt.Errorf("error unmarshalling existing resource inclusions from YAML: %v", err)
		}
	}
	resourceMap := make(map[string]map[string]struct{})
	for _, inclusion := range existingResourceInclusions {
		if len(inclusion.APIGroups) == 0 {
			continue
		}
		apiGroup := inclusion.APIGroups[0]
		if _, found := resourceMap[apiGroup]; !found {
			resourceMap[apiGroup] = make(map[string]struct{})
		}
		for _, kind := range inclusion.Kinds {
			resourceMap[apiGroup][kind] = struct{}{}
		}
	}
	changeDetected := false
	for apiGroup, kinds := range groupVersion {
		if _, exists := resourceMap[apiGroup]; !exists {
			resourceMap[apiGroup] = make(map[string]struct{})
			changeDetected = true
		}
		for _, kind := range kinds {
			if _, kindExists := resourceMap[apiGroup][kind]; !kindExists {
				resourceMap[apiGroup][kind] = struct{}{}
				changeDetected = true
			}
		}
	}
	if !changeDetected {
		klog.Infof("No changes detected in resource inclusions. ConfigMap update not required.")
		return nil
	}

	updatedResourceInclusions := make([]ResourceInclusion, 0, len(resourceMap))
	for apiGroup, kindsSet := range resourceMap {
		kinds := make([]string, 0, len(kindsSet))
		for kind := range kindsSet {
			kinds = append(kinds, kind)
		}
		updatedResourceInclusions = append(updatedResourceInclusions, ResourceInclusion{
			APIGroups: []string{apiGroup},
			Kinds:     kinds,
			Clusters:  []string{"*"},
		})
	}
	newYamlData, err := yaml.Marshal(updatedResourceInclusions)
	if err != nil {
		return fmt.Errorf("error marshalling updated resource inclusions to YAML: %v", err)
	}
	if configMap.Data == nil {
		configMap.Data = make(map[string]string)
	}
	configMap.Data["resource.inclusions"] = string(newYamlData)
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, updateErr := k8sclient.CoreV1().ConfigMaps("argocd").Update(context.Background(), configMap, v1.UpdateOptions{})
		return updateErr
	})
	if err != nil {
		return fmt.Errorf("error updating ConfigMap: %v", err)
	}
	klog.Infof("Resource inclusions updated successfully in argocd-cm ConfigMap.")
	return nil
}

// GroupResourcesByAPIGroup processes the parent-child relationship map and groups resources by their API group and kind.
func groupResourcesByAPIGroup(resourceTree map[string][]string) map[string][]string {
	groupedResources := make(map[string]map[string]struct{})
	for parent, children := range resourceTree {
		parentGroup := strings.Split(parent, "_")[0]
		parentKind := strings.Split(parent, "_")[1]
		if _, exists := groupedResources[parentGroup]; !exists {
			groupedResources[parentGroup] = make(map[string]struct{})
		}
		groupedResources[parentGroup][parentKind] = struct{}{}
		for _, child := range children {
			childGroup := strings.Split(child, "_")[0]
			childKind := strings.Split(child, "_")[1]

			if _, exists := groupedResources[childGroup]; !exists {
				groupedResources[childGroup] = make(map[string]struct{})
			}
			groupedResources[childGroup][childKind] = struct{}{}
		}
	}
	// Convert map of sets to map of slices
	result := make(map[string][]string)
	for group, kindsSet := range groupedResources {
		if group == "core" {
			group = ""
		}
		for kind := range kindsSet {
			result[group] = append(result[group], kind)
		}
	}
	return result
}

func updateResourceRelationLookup(destinationConfig *rest.Config, namespace string, k8sclient kubernetes.Interface) (map[string]string, error) {
	ctx := context.Background()
	resourcesRelation, err := resourcegraph.GetResourcesRelation(ctx, destinationConfig)
	if err != nil {
		klog.Errorf("failed to update resource-relation-lookup ConfigMap: %v", err)
		return nil, err
	}
	convertedResourcesRelation := make(map[string]string)
	for k, v := range resourcesRelation {
		convertedResourcesRelation[k] = strings.Join(v, ",")
	}
	configMap, err := k8sclient.CoreV1().ConfigMaps(namespace).Get(ctx, RESOURCE_RELATION_LOOKUP, v1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch ConfigMap for update: %v", err)
	}
	configMap.Data = convertedResourcesRelation
	_, err = k8sclient.CoreV1().ConfigMaps(namespace).Update(ctx, configMap, v1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to update resource-relation-lookup ConfigMap: %v", err)
	}
	return convertedResourcesRelation, nil
}
