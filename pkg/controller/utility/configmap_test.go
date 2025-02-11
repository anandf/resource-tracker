package utility

import (
	"context"
	"reflect"
	"sort"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestGroupResourcesByAPIGroup(t *testing.T) {
	tests := []struct {
		name           string
		resourceTree   map[string][]string
		expectedResult map[string][]string
	}{
		{
			name: "single group with multiple kinds",
			resourceTree: map[string][]string{
				"apps_Deployment": {"apps_ReplicaSet", "core_Pod"},
				"apps_ReplicaSet": {"core_Pod"},
			},
			expectedResult: map[string][]string{
				"apps": {"Deployment", "ReplicaSet"},
				"core": {"Pod"},
			},
		},
		{
			name: "multiple groups with multiple kinds",
			resourceTree: map[string][]string{
				"core_Node":       {"core_Pod"},
				"core_Pod":        {"core_Container"},
				"apps_Deployment": {"apps_ReplicaSet"},
			},
			expectedResult: map[string][]string{
				"core": {"Node", "Pod", "Container"},
				"apps": {"Deployment", "ReplicaSet"},
			},
		},
		{
			name: "single group with single kind",
			resourceTree: map[string][]string{
				"core_Pod": {"core_Container"},
			},
			expectedResult: map[string][]string{
				"core": {"Pod", "Container"},
			},
		},
		{
			name:           "empty resource tree",
			resourceTree:   map[string][]string{},
			expectedResult: map[string][]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := groupResourcesByAPIGroup(tt.resourceTree)
			// Sort both expected and actual values before comparing
			for key := range result {
				sort.Strings(result[key])
			}
			for key := range tt.expectedResult {
				sort.Strings(tt.expectedResult[key])
			}
			if !reflect.DeepEqual(result, tt.expectedResult) {
				t.Errorf("groupResourcesByAPIGroup() = %v, expected %v", result, tt.expectedResult)
			}
		})
	}
}
func TestUpdateResourceInclusion(t *testing.T) {
	tests := []struct {
		name         string
		resourceTree map[string][]string
		existingData map[string]string
		expectedData map[string]string
		expectError  bool
	}{
		{
			name: "update with new resources",
			resourceTree: map[string][]string{
				"apps_Deployment": {"apps_ReplicaSet", "core_Pod"},
				"apps_ReplicaSet": {"core_Pod"},
			},
			existingData: map[string]string{
				"resource.inclusions": `
- apiGroups:
  - apps
  kinds:
  - Deployment
`,
			},
			expectedData: map[string]string{
				"resource.inclusions": `- apiGroups:
  - apps
  kinds:
  - Deployment
  - ReplicaSet
- apiGroups:
  - core
  kinds:
  - Pod
`,
			},
			expectError: false,
		},
		{
			name: "no changes detected",
			resourceTree: map[string][]string{
				"apps_Deployment": {"apps_ReplicaSet"},
			},
			existingData: map[string]string{
				"resource.inclusions": `
- apiGroups:
  - apps
  kinds:
  - Deployment
  - ReplicaSet
`,
			},
			expectedData: map[string]string{
				"resource.inclusions": `
- apiGroups:
  - apps
  kinds:
  - Deployment
  - ReplicaSet
`,
			},
			expectError: false,
		},
		{
			name: "error fetching ConfigMap",
			resourceTree: map[string][]string{
				"apps_Deployment": {"apps_ReplicaSet"},
			},
			existingData: nil,
			expectedData: nil,
			expectError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			if tt.existingData != nil {
				configMap := &v1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      ConfigMapName,
						Namespace: "argocd",
					},
					Data: tt.existingData,
				}
				client.CoreV1().ConfigMaps("argocd").Create(context.Background(), configMap, metav1.CreateOptions{})
			}
			err := UpdateResourceInclusion(tt.resourceTree, client)
			if (err != nil) != tt.expectError {
				t.Errorf("UpdateResourceInclusion() error = %v, expectError %v", err, tt.expectError)
				return
			}
			if !tt.expectError {
				configMap, err := client.CoreV1().ConfigMaps("argocd").Get(context.Background(), ConfigMapName, metav1.GetOptions{})
				if err != nil {
					t.Errorf("Error fetching ConfigMap: %v", err)
					return
				}
				if !reflect.DeepEqual(configMap.Data, tt.expectedData) {
					t.Errorf("ConfigMap data = %v, expected %v", configMap.Data, tt.expectedData)
				}
			}
		})
	}
}
