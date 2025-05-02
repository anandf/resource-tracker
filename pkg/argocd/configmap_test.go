package argocd

import (
	"context"
	"reflect"
	"testing"

	"github.com/anandf/resource-tracker/pkg/kube"
	"github.com/emirpasic/gods/sets/hashset"
	"github.com/stretchr/testify/assert"
	"gopkg.in/yaml.v2"
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
			for apiGroup, kinds := range tt.expectedResult {
				if _, exist := result[apiGroup]; !exist {
					assert.Failf(t, "Expected API group %s not found in groupResourses %v", apiGroup, result)
				}
				for _, kind := range kinds {
					if !result[apiGroup].Contains(kind) {
						assert.Failf(t, "Kind mismatch", "Expected kind %s not found in API group %s. Actual kinds: %v", kind, apiGroup, result[apiGroup].Values())
					}
				}
			}
		})
	}
}

func TestUpdateResourceInclusion(t *testing.T) {
	tests := []struct {
		name              string
		namespace         string
		existingConfigMap *v1.ConfigMap
		trackedResources  map[string]*hashset.Set
		expectedConfigMap *v1.ConfigMap
		expectError       bool
	}{
		{
			name:      "update with new inclusions",
			namespace: "argocd",
			existingConfigMap: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ARGOCD_CM,
					Namespace: "argocd",
				},
				Data: map[string]string{
					"resource.inclusions": `
- apiGroups:
  - apps
  kinds:
  - Deployment
`,
				},
			},
			trackedResources: map[string]*hashset.Set{
				"apps": hashset.New("Deployment", "ReplicaSet"),
				"core": hashset.New("Pod"),
			},
			expectedConfigMap: &v1.ConfigMap{
				Data: map[string]string{
					"resource.inclusions": `- apiGroups:
  - apps
  kinds:
  - Deployment
  - ReplicaSet
  clusters:
  - '*'
- apiGroups:
  - core
  kinds:
  - Pod
  clusters:
  - '*'
`,
				},
			},
			expectError: false,
		},
		{
			name:      "no changes detected",
			namespace: "argocd",
			existingConfigMap: &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ARGOCD_CM,
					Namespace: "argocd",
				},
				Data: map[string]string{
					"resource.inclusions": `
- apiGroups:
  - apps
  kinds:
  - Deployment
  - ReplicaSet
  clusters:
  - '*'
- apiGroups:
  - core
  kinds:
  - Pod
  clusters:
  - '*'
`,
				},
			},
			trackedResources: map[string]*hashset.Set{
				"apps": hashset.New("Deployment", "ReplicaSet"),
				"core": hashset.New("Pod"),
			},
			expectedConfigMap: nil,
			expectError:       false,
		},
		{
			name:              "error fetching ConfigMap",
			namespace:         "argocd",
			existingConfigMap: nil,
			trackedResources: map[string]*hashset.Set{
				"apps": hashset.New("Deployment"),
			},
			expectedConfigMap: nil,
			expectError:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			if tt.existingConfigMap != nil {
				_, err := client.CoreV1().ConfigMaps(tt.namespace).Create(context.Background(), tt.existingConfigMap, metav1.CreateOptions{})
				assert.NoError(t, err)
			}

			argocd := &argocd{
				kubeClient:       &kube.KubeClient{Clientset: client, Namespace: tt.namespace},
				trackedResources: tt.trackedResources,
			}
			err := argocd.UpdateResourceInclusion(tt.namespace)
			if (err != nil) != tt.expectError {
				t.Errorf("UpdateResourceInclusion() error = %v, expectError %v", err, tt.expectError)
				return
			}

			if !tt.expectError && tt.expectedConfigMap != nil {
				configMap, err := client.CoreV1().ConfigMaps(tt.namespace).Get(context.Background(), ARGOCD_CM, metav1.GetOptions{})
				assert.NoError(t, err)

				expectedMap, err := yamlToMap(tt.expectedConfigMap.Data["resource.inclusions"])
				assert.NoError(t, err)

				actualMap, err := yamlToMap(configMap.Data["resource.inclusions"])
				assert.NoError(t, err)

				assert.True(t, reflect.DeepEqual(expectedMap, actualMap), "ConfigMap data mismatch. Expected: %v, Got: %v", expectedMap, actualMap)
			}
		})
	}
}
func TestHasResourceInclusionChanged(t *testing.T) {
	tests := []struct {
		name     string
		a        map[string]*hashset.Set
		b        map[string]*hashset.Set
		expected bool
	}{
		{
			name: "identical maps",
			a: map[string]*hashset.Set{
				"apps": hashset.New("Deployment", "ReplicaSet"),
				"core": hashset.New("Pod"),
			},
			b: map[string]*hashset.Set{
				"apps": hashset.New("Deployment", "ReplicaSet"),
				"core": hashset.New("Pod"),
			},
			expected: true,
		},
		{
			name: "different sizes",
			a: map[string]*hashset.Set{
				"apps": hashset.New("Deployment"),
			},
			b: map[string]*hashset.Set{
				"apps": hashset.New("Deployment", "ReplicaSet"),
			},
			expected: false,
		},
		{
			name: "different keys",
			a: map[string]*hashset.Set{
				"apps": hashset.New("Deployment"),
			},
			b: map[string]*hashset.Set{
				"core": hashset.New("Pod"),
			},
			expected: false,
		},
		{
			name: "different values in same key",
			a: map[string]*hashset.Set{
				"apps": hashset.New("Deployment"),
			},
			b: map[string]*hashset.Set{
				"apps": hashset.New("ReplicaSet"),
			},
			expected: false,
		},
		{
			name:     "empty maps",
			a:        map[string]*hashset.Set{},
			b:        map[string]*hashset.Set{},
			expected: true,
		},
		{
			name: "one map empty",
			a: map[string]*hashset.Set{
				"apps": hashset.New("Deployment"),
			},
			b:        map[string]*hashset.Set{},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hasResourceInclusionChanged(tt.a, tt.b)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// Convert YAML string to map for comparison
func yamlToMap(yamlStr string) (map[string]interface{}, error) {
	var result []map[string]interface{}
	err := yaml.Unmarshal([]byte(yamlStr), &result)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"resource.inclusions": result}, nil
}
