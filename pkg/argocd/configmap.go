package argocd

import (
	"context"
	"fmt"
	"strings"

	"maps"

	"github.com/anandf/resource-tracker/pkg/resourcegraph"
	"github.com/emirpasic/gods/sets/hashset"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	ARGOCD_CM                = "argocd-cm"
	RESOURCE_RELATION_LOOKUP = "resource-relation-lookup"
)

type resourceInclusion struct {
	APIGroups []string `yaml:"apiGroups"`
	Kinds     []string `yaml:"kinds"`
	Clusters  []string `yaml:"clusters"`
}

func updateresourceInclusion(resourceTree map[string][]string, k8sclient kubernetes.Interface, namespace string) error {
	ctx := context.Background()
	groupVersion := groupResourcesByAPIGroup(resourceTree)
	log.Info("groupVersion: ", groupVersion)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		configMap, err := k8sclient.CoreV1().ConfigMaps(namespace).Get(ctx, ARGOCD_CM, v1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error fetching ConfigMap: %v", err)
		}
		var existingresourceInclusions []resourceInclusion
		if existingYamlData, exists := configMap.Data["resource.inclusions"]; exists {
			if err := yaml.Unmarshal([]byte(existingYamlData), &existingresourceInclusions); err != nil {
				return fmt.Errorf("error unmarshalling existing resource inclusions from YAML: %v", err)
			}
		}
		resourceMap := make(map[string]*hashset.Set)
		// Populate resourceMap from existing resource inclusions
		for _, inclusion := range existingresourceInclusions {
			if len(inclusion.APIGroups) == 0 {
				continue
			}
			apiGroup := inclusion.APIGroups[0]
			if _, found := resourceMap[apiGroup]; !found {
				resourceMap[apiGroup] = hashset.New()
			}
			for _, kind := range inclusion.Kinds {
				resourceMap[apiGroup].Add(kind)
			}
		}
		changeDetected := false
		// Compare with groupVersion and update resourceMap
		for apiGroup, kinds := range groupVersion {
			if _, exists := resourceMap[apiGroup]; !exists {
				resourceMap[apiGroup] = hashset.New()
				changeDetected = true
			}
			for _, kind := range kinds {
				if !resourceMap[apiGroup].Contains(kind) {
					resourceMap[apiGroup].Add(kind)
					changeDetected = true
				}
			}
		}
		if !changeDetected {
			log.Infof("No changes detected in resource inclusions. ConfigMap update not required.")
			return nil
		}
		// prepare
		// Prepare the updated resource inclusions based on the resourceMap
		updatedresourceInclusions := make([]resourceInclusion, 0, len(resourceMap))
		for apiGroup, kindsSet := range resourceMap {
			kinds := make([]string, 0, kindsSet.Size())
			for _, kind := range kindsSet.Values() {
				kinds = append(kinds, kind.(string))
			}
			updatedresourceInclusions = append(updatedresourceInclusions, resourceInclusion{
				APIGroups: []string{apiGroup},
				Kinds:     kinds,
				Clusters:  []string{"*"},
			})
		}
		newYamlData, err := yaml.Marshal(updatedresourceInclusions)
		if err != nil {
			return fmt.Errorf("error marshalling updated resource inclusions to YAML: %v", err)
		}
		if configMap.Data == nil {
			configMap.Data = make(map[string]string)
		}
		configMap.Data["resource.inclusions"] = string(newYamlData)
		_, err = k8sclient.CoreV1().ConfigMaps(namespace).Update(ctx, configMap, v1.UpdateOptions{})
		if err != nil {
			log.Warningf("Retrying due to conflict: %v", err)
			return err
		}
		log.Infof("Resource inclusions updated successfully in argocd-cm ConfigMap.")
		return nil
	})
}

// GroupResourcesByAPIGroup processes the parent-child relationship map and groups resources by their API group and kind.
func groupResourcesByAPIGroup(resourceTree map[string][]string) map[string][]string {
	groupedResources := make(map[string]*hashset.Set)
	for parent, children := range resourceTree {
		parentGroup := strings.Split(parent, "_")[0]
		parentKind := strings.Split(parent, "_")[1]
		if _, exists := groupedResources[parentGroup]; !exists {
			groupedResources[parentGroup] = hashset.New()
		}
		groupedResources[parentGroup].Add(parentKind)
		for _, child := range children {
			childGroup := strings.Split(child, "_")[0]
			childKind := strings.Split(child, "_")[1]
			if _, exists := groupedResources[childGroup]; !exists {
				groupedResources[childGroup] = hashset.New()
			}
			groupedResources[childGroup].Add(childKind)
		}
	}
	result := make(map[string][]string)
	for group, kindsSet := range groupedResources {
		if group == "core" {
			group = ""
		}
		kinds := make([]string, 0, kindsSet.Size())
		for _, kind := range kindsSet.Values() {
			kinds = append(kinds, kind.(string))
		}
		result[group] = kinds
	}
	return result
}

func updateResourceRelationLookup(resourcemapper *resourcegraph.ResourceMapper, namespace string, k8sclient kubernetes.Interface) (map[string]string, error) {
	ctx := context.Background()
	resourcesRelation, err := resourcemapper.GetResourcesRelation(ctx)
	if err != nil {
		log.Errorf("failed to update resource-relation-lookup ConfigMap: %v", err)
		return nil, err
	}
	convertedResourcesRelation := make(map[string]string)
	for k, v := range resourcesRelation {
		values := v.Values() // Avoid multiple calls
		strValues := make([]string, len(values))
		for i, val := range values {
			strValues[i] = val.(string)
		}
		convertedResourcesRelation[k] = strings.Join(strValues, ",")
	}
	// Use retry logic to handle update conflicts
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch the latest version inside the retry loop to avoid stale reads
		configMap, err := k8sclient.CoreV1().ConfigMaps(namespace).Get(ctx, RESOURCE_RELATION_LOOKUP, v1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to fetch ConfigMap for update: %v", err)
		}
		if configMap.Data == nil {
			configMap.Data = make(map[string]string)
		}
		maps.Copy(configMap.Data, convertedResourcesRelation)
		_, err = k8sclient.CoreV1().ConfigMaps(namespace).Update(ctx, configMap, v1.UpdateOptions{})
		if err != nil {
			log.Warnf("Retrying due to conflict: %v", err)
			return err
		}
		return nil
	})
	if err != nil {
		log.Errorf("Final failure updating ConfigMap: %v", err)
		return nil, err
	}
	log.Infof("Resource Relations updated successfully in resource-relation-lookup ConfigMap.")
	return convertedResourcesRelation, nil
}
