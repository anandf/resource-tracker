package kube

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGetResourceRelation(t *testing.T) {
	tests := []struct {
		name      string
		configMap map[string]string
		resources []*unstructured.Unstructured
		expected  map[string][]string
	}{
		{
			name: "single resource with children",
			configMap: map[string]string{
				"apps_Deployment": "apps_ReplicaSet,core_Pod",
				"apps_ReplicaSet": "core_Pod",
			},
			resources: []*unstructured.Unstructured{
				{
					Object: map[string]interface{}{
						"apiVersion": "apps/v1",
						"kind":       "Deployment",
					},
				},
			},
			expected: map[string][]string{
				"apps_Deployment": {"apps_ReplicaSet", "core_Pod"},
				"apps_ReplicaSet": {"core_Pod"},
			},
		},
		{
			name: "multiple resources with nested children",
			configMap: map[string]string{
				"core_Node": "core_Pod",
				"core_Pod":  "core_Container",
			},
			resources: []*unstructured.Unstructured{
				{
					Object: map[string]interface{}{
						"apiVersion": "v1",
						"kind":       "Node",
					},
				},
			},
			expected: map[string][]string{
				"core_Node": {"core_Pod"},
				"core_Pod":  {"core_Container"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetResourceRelation(tt.configMap, tt.resources)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("GetResourceRelation() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestBuildResourceTree(t *testing.T) {
	tests := []struct {
		name           string
		configMap      map[string]string
		resourceKey    string
		expectedResult map[string][]string
	}{
		{
			name: "single level tree",
			configMap: map[string]string{
				"apps_Deployment": "apps_ReplicaSet,core_Pod",
				"apps_ReplicaSet": "core_Pod",
			},
			resourceKey: "apps_Deployment",
			expectedResult: map[string][]string{
				"apps_Deployment": {"apps_ReplicaSet", "core_Pod"},
				"apps_ReplicaSet": {"core_Pod"},
			},
		},
		{
			name: "multi-level tree",
			configMap: map[string]string{
				"core_Node": "core_Pod",
				"core_Pod":  "core_Container",
			},
			resourceKey: "core_Node",
			expectedResult: map[string][]string{
				"core_Node": {"core_Pod"},
				"core_Pod":  {"core_Container"},
			},
		},
		{
			name: "circular dependency",
			configMap: map[string]string{
				"core_A": "core_B",
				"core_B": "core_A",
			},
			resourceKey: "core_A",
			expectedResult: map[string][]string{
				"core_A": {"core_B"},
				"core_B": {"core_A"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := make(map[string][]string)
			buildResourceTree(tt.configMap, tt.resourceKey, result, make(map[string]struct{}))
			if !reflect.DeepEqual(result, tt.expectedResult) {
				t.Errorf("buildResourceTree() = %v, expected %v", result, tt.expectedResult)
			}
		})
	}
}
