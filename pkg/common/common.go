package common

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v2"
)

type Void struct{}
type ResourceInfoSet map[ResourceInfo]Void
type ResourceInfo struct {
	Kind      string
	Group     string
	Name      string
	Namespace string
}

type Kinds map[string]Void
type GroupedResourceKinds map[string]Kinds

type ResourceInclusionEntry struct {
	APIGroups []string `json:"apiGroups,omitempty"`
	Kinds     []string `json:"kinds,omitempty"`
	Clusters  []string `json:"clusters,omitempty"`
}

func (r *ResourceInfo) String() string {
	return fmt.Sprintf("[group:%s, kind: %s, name: %s, namespace:%s]", r.Group, r.Kind, r.Name, r.Namespace)
}

func (k *Kinds) Equal(other *Kinds) bool {
	if len(*k) != len(*other) {
		return false
	}
	for key := range *k {
		if _, ok := (*other)[key]; !ok {
			return false
		}
	}
	return true
}

func (r *ResourceInclusionEntry) Equal(other *ResourceInclusionEntry) bool {
	if !reflect.DeepEqual(r.APIGroups, other.APIGroups) || !reflect.DeepEqual(r.Clusters, other.Clusters) || len(r.Kinds) != len(other.Kinds) {
		return false
	}
	currentKindsStr := fmt.Sprintf("%v", r.Kinds)
	for _, otherKind := range other.Kinds {
		if !strings.Contains(currentKindsStr, otherKind) {
			return false
		}
	}
	return true
}

// String is the single, centralized function to print the YAML output.
func (g *GroupedResourceKinds) String() string {
	includedResources := make([]ResourceInclusionEntry, 0, len(*g))
	for group, kinds := range *g {
		// Handle core group
		apiGroup := group
		if group == "core" || group == "" {
			apiGroup = ""
		}

		includedResources = append(includedResources, ResourceInclusionEntry{
			APIGroups: []string{apiGroup},
			Kinds:     getUniqueKinds(kinds),
			Clusters:  []string{"*"},
		})
	}
	out, err := yaml.Marshal(includedResources)
	if err != nil {
		return fmt.Sprintf("error: %v", err.Error())
	}
	return string(out)
}

// Equal returns true if any of the resource inclusions entries is modified, false otherwise
func (g *GroupedResourceKinds) Equal(other *GroupedResourceKinds) bool {
	if len(*other) != len(*g) {
		return false
	}
	for otherGroupName, otherKinds := range *other {
		if _, ok := (*g)[otherGroupName]; !ok {
			return false
		}
		currentKinds, _ := (*g)[otherGroupName]
		if !currentKinds.Equal(&otherKinds) {
			return false
		}
	}
	return true
}

func (r *ResourceInfoSet) String() string {
	resourceInfos := make([]string, 0, len(*r))
	for resInfo := range *r {
		resourceInfos = append(resourceInfos, resInfo.String())
	}
	return "{" + strings.Join(resourceInfos, ", ") + "}"
}

func (g *GroupedResourceKinds) FromYaml(resourceInclusionsYaml string) error {
	var existingResourceInclusionsInCM []ResourceInclusionEntry
	err := yaml.Unmarshal([]byte(resourceInclusionsYaml), &existingResourceInclusionsInCM)
	if err != nil {
		return err
	}
	for _, resourceInclusion := range existingResourceInclusionsInCM {
		for _, apiGroup := range resourceInclusion.APIGroups {
			group := apiGroup
			if group == "" {
				group = "core"
			}
			for _, kind := range resourceInclusion.Kinds {
				if (*g)[group] == nil {
					(*g)[group] = make(map[string]Void)
				}
				(*g)[group][kind] = Void{}
			}
			// break after the first item in apiGroup list
			break
		}
	}
	return nil
}

// MergeResourceInfos groups given set of ResourceInfo objects according to their api groups and merges it into this GroupResourceKinds object
func (g *GroupedResourceKinds) MergeResourceInfos(input []*ResourceInfo) {
	for _, resourceInfo := range input {
		apiGroup := resourceInfo.Group
		if apiGroup == "" {
			apiGroup = "core"
		}

		if _, found := (*g)[apiGroup]; !found {
			(*g)[apiGroup] = map[string]Void{
				resourceInfo.Kind: {},
			}
		} else {
			(*g)[apiGroup][resourceInfo.Kind] = Void{}
		}
	}
}

// getUniqueKinds given a set of kinds, it returns unique set of kinds
func getUniqueKinds(kinds Kinds) []string {
	uniqueKinds := make([]string, 0)
	for kind := range kinds {
		uniqueKinds = append(uniqueKinds, kind)
	}
	return uniqueKinds
}
