package utility

import (
	"context"
	"fmt"
	"strings"

	"gopkg.in/yaml.v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

const (
	// ConfigMapName is the name of the configmap that stores the resource inclusion status.
	ConfigMapName = "argocd-cm"
)

func UpdateResourceInclusion(resourceTree map[string][]string, k8sclient kubernetes.Interface) error {
	// Fetch the ConfigMap from the argocd namespace
	configMap, err := k8sclient.CoreV1().ConfigMaps("argocd").Get(context.Background(), ConfigMapName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error fetching ConfigMap: %v", err)
	}

	groupVersion := groupResourcesByAPIGroup(resourceTree)

	// Parse the existing resource inclusions
	var existingResourceInclusions []map[string]interface{}
	if existingYamlData, exists := configMap.Data["resource.inclusions"]; exists {
		if err := yaml.Unmarshal([]byte(existingYamlData), &existingResourceInclusions); err != nil {
			return fmt.Errorf("error unmarshalling existing resource inclusions from YAML: %v", err)
		}
	}

	// Convert existing data into a lookup map for quick checks
	resourceMap := make(map[string]map[string]struct{})
	for _, inclusion := range existingResourceInclusions {
		apiGroups, _ := inclusion["apiGroups"].([]interface{})
		kinds, _ := inclusion["kinds"].([]interface{})

		if len(apiGroups) == 0 {
			continue
		}
		apiGroup := apiGroups[0].(string)
		if _, found := resourceMap[apiGroup]; !found {
			resourceMap[apiGroup] = make(map[string]struct{})
		}
		for _, kind := range kinds {
			resourceMap[apiGroup][kind.(string)] = struct{}{}
		}
	}

	changeDetected := false
	for apiGroup, kinds := range groupVersion {
		if _, exists := resourceMap[apiGroup]; !exists {
			resourceMap[apiGroup] = make(map[string]struct{})
			changeDetected = true
		}
		for _, kind := range kinds {
			if _, exists := resourceMap[apiGroup][kind]; !exists {
				resourceMap[apiGroup][kind] = struct{}{}
				changeDetected = true
			}
		}
	}

	// If no changes were detected, return early
	if !changeDetected {
		klog.Infof("No changes detected in resource inclusions. ConfigMap update not required.")
		return nil
	}

	// Convert back to the required format
	updatedResourceInclusions := make([]map[string]interface{}, 0, len(resourceMap))
	for apiGroup, kindsSet := range resourceMap {
		kinds := make([]string, 0, len(kindsSet))
		for kind := range kindsSet {
			kinds = append(kinds, kind)
		}
		updatedResourceInclusions = append(updatedResourceInclusions, map[string]interface{}{
			"apiGroups": []string{apiGroup},
			"kinds":     kinds,
		})
	}

	// Convert the new resource inclusion data to YAML
	newYamlData, err := yaml.Marshal(updatedResourceInclusions)
	if err != nil {
		return fmt.Errorf("error marshalling updated resource inclusions to YAML: %v", err)
	}

	// Update the ConfigMap data
	if configMap.Data == nil {
		configMap.Data = make(map[string]string)
	}
	configMap.Data["resource.inclusions"] = string(newYamlData)

	// Retry to update the ConfigMap in case of conflicts
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, updateErr := k8sclient.CoreV1().ConfigMaps("argocd").Update(context.Background(), configMap, metav1.UpdateOptions{})
		return updateErr
	})

	if err != nil {
		return fmt.Errorf("error updating ConfigMap: %v", err)
	}

	klog.Infof("ConfigMap updated successfully.")
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
		for kind := range kindsSet {
			result[group] = append(result[group], kind)
		}
	}

	return result
}
