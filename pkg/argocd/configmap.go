package argocd

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"maps"

	"github.com/emirpasic/gods/sets/hashset"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func (a *argocd) UpdateResourceInclusion(namespace string) error {
	ctx := context.Background()
	if namespace == "" {
		namespace = a.kubeClient.Namespace
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		configMap, err := a.kubeClient.Clientset.CoreV1().ConfigMaps(namespace).Get(ctx, ARGOCD_CM, v1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error fetching ConfigMap: %v", err)
		}
		var existingresourceInclusions []resourceInclusion
		if existingYamlData, exists := configMap.Data["resource.inclusions"]; exists {
			if err := yaml.Unmarshal([]byte(existingYamlData), &existingresourceInclusions); err != nil {
				return fmt.Errorf("error unmarshalling existing resource inclusions from YAML: %v", err)
			}
		}
		// Convert existingresourceInclusions into a map
		existingMap := make(map[string]*hashset.Set)
		changeDetected := false
		for _, inclusion := range existingresourceInclusions {
			if len(inclusion.APIGroups) == 0 {
				continue
			}
			apiGroup := inclusion.APIGroups[0]
			if apiGroup == "" {
				apiGroup = "core"
			}
			if _, found := existingMap[apiGroup]; !found {
				existingMap[apiGroup] = hashset.New()
			}

			for _, kind := range inclusion.Kinds {
				existingMap[apiGroup].Add(kind)
			}
			// Detect if there are multiple API groups in a single inclusion
			if len(inclusion.APIGroups) > 1 {
				changeDetected = true
			}
		}
		// Skip update if no changes are detected and the maps are equal
		if !changeDetected && hasResourceInclusionChanged(a.trackedResources, existingMap) {
			log.Infof("No changes detected in the resource inclusions of the argocd-cm ConfigMap. Update skipped.")
			return nil
		}
		// Prepare the updated resource inclusions based on the resourceMap
		updatedresourceInclusions := make([]resourceInclusion, 0, len(a.trackedResources))
		for apiGroup, kindsSet := range a.trackedResources {
			if apiGroup == "core" {
				apiGroup = ""
			}
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
		_, err = a.kubeClient.Clientset.CoreV1().ConfigMaps(namespace).Update(ctx, configMap, v1.UpdateOptions{})
		if err != nil {
			log.Warningf("Retrying due to conflict: %v", err)
			return err
		}
		log.Infof("Resource inclusions updated successfully in argocd-cm ConfigMap.")
		return nil
	})
}

// GroupResourcesByAPIGroup processes the parent-child relationship map and groups resources by their API group and kind.
func groupResourcesByAPIGroup(resourceTree map[string][]string) map[string]*hashset.Set {
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

	return groupedResources
}

func (a *argocd) updateResourceRelationLookup(namespace, resourceMapperKey, appName string) (map[string]string, error) {
	a.mapperMutex.Lock()
	defer a.mapperMutex.Unlock()
	ctx := context.Background()
	// Retrieve the resource relation data
	resourcesRelation, err := a.resourceMapperStore[resourceMapperKey].GetResourcesRelation(ctx)
	if err != nil {
		log.Errorf("failed to update resource-relation-lookup ConfigMap: %v", err)
		return nil, err
	}
	// Build a new map with sorted values for deterministic ordering
	convertedResourcesRelation := make(map[string]string)
	for k, v := range resourcesRelation {
		values := v.Values() // Avoid multiple calls
		strValues := make([]string, len(values))
		for i, val := range values {
			strValues[i] = val.(string)
		}
		// Sort to ensure the same order every time
		sort.Strings(strValues)
		convertedResourcesRelation[k] = strings.Join(strValues, ",")
	}
	// Use retry logic to handle update conflicts
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch the latest version to avoid stale reads
		configMap, err := a.kubeClient.Clientset.CoreV1().ConfigMaps(namespace).Get(ctx, RESOURCE_RELATION_LOOKUP, v1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to fetch ConfigMap for update: %v", err)
		}
		if configMap.Data == nil {
			configMap.Data = make(map[string]string)
		}
		// Compare the existing data with the new data.
		// This avoids an update if nothing has changed.
		if reflect.DeepEqual(configMap.Data, convertedResourcesRelation) {
			log.Infof("No changes detected in resource-relation-lookup ConfigMap for app: %s. Update not required.", appName)
			return nil
		}
		// Update the ConfigMap with the new data
		maps.Copy(configMap.Data, convertedResourcesRelation)
		_, err = a.kubeClient.Clientset.CoreV1().ConfigMaps(namespace).Update(ctx, configMap, v1.UpdateOptions{})
		if err != nil {
			log.Warnf("Retrying due to conflict: %v", err)
			return err
		}
		log.Infof("Resource Relations updated successfully in resource-relation-lookup ConfigMap for app: %s.", appName)
		return nil
	})
	if err != nil {
		log.Errorf("Final failure updating ConfigMap: %v", err)
		return nil, err
	}
	return convertedResourcesRelation, nil
}

func hasResourceInclusionChanged(a, b map[string]*hashset.Set) bool {
	if len(a) != len(b) {
		return false
	}
	for apiGroup, kindsA := range a {
		kindsB, ok := b[apiGroup]
		if !ok {
			return false
		}
		if kindsA.Size() != kindsB.Size() {
			return false
		}
		for _, kind := range kindsA.Values() {
			if !kindsB.Contains(kind) {
				return false
			}
		}
	}
	return true
}
