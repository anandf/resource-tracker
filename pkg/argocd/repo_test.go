package argocd

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v2/reposerver/apiclient"
	mockrepoclient "github.com/argoproj/argo-cd/v2/reposerver/apiclient/mocks"
	dbmocks "github.com/argoproj/argo-cd/v2/util/db/mocks"
	"github.com/argoproj/argo-cd/v2/util/settings"
	"github.com/argoproj/gitops-engine/pkg/utils/kube"
	"github.com/argoproj/gitops-engine/pkg/utils/kube/kubetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

type MockKubectl struct {
	mock.Mock
	kube.Kubectl
}

func (m *MockKubectl) GetServerVersion(config *rest.Config) (string, error) {
	args := m.Called(config)
	return args.String(0), args.Error(1)
}

func (m *MockKubectl) GetAPIResources(config *rest.Config, all bool, filter kube.ResourceFilter) ([]kube.APIResourceInfo, error) {
	args := m.Called(config, all, filter)
	return args.Get(0).([]kube.APIResourceInfo), args.Error(1)
}

func TestGetApplicationChildManifests(t *testing.T) {
	ctx := context.Background()
	configMap := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "argocd-cm",
			Namespace: "test-argocd",
			Labels: map[string]string{
				"app.kubernetes.io/part-of": "argocd",
			},
		},
		Data: make(map[string]string),
	}
	setupMocks := func() (*MockKubectl, *dbmocks.ArgoDB, *settings.SettingsManager, *mockrepoclient.Clientset) {
		mockKubectl := &MockKubectl{Kubectl: &kubetest.MockKubectlCmd{}}
		mockKubectl.On("GetServerVersion", mock.Anything).Return("v1.25.0", nil)
		mockKubectl.On("GetAPIResources", mock.Anything, false, &settings.ResourcesFilter{}).Return(
			[]kube.APIResourceInfo{
				{GroupKind: schema.GroupKind{Group: "", Kind: "Pod"}},
			}, nil)

		kubeClient := fake.NewSimpleClientset(&configMap)
		db := &dbmocks.ArgoDB{}
		settingsManager := settings.NewSettingsManager(ctx, kubeClient, "test-argocd")
		mockRepoClient := mockrepoclient.RepoServerServiceClient{}
		mockRepoClientset := mockrepoclient.Clientset{RepoServerServiceClient: &mockRepoClient}
		db.On("ListHelmRepositories", ctx).Return([]*v1alpha1.Repository{}, nil)
		db.On("GetAllHelmRepositoryCredentials", ctx).Return([]*v1alpha1.RepoCreds{}, nil)
		db.On("GetCluster", ctx, mock.Anything).Return(&v1alpha1.Cluster{}, nil)
		db.On("GetRepository", ctx, mock.Anything, mock.Anything).Return(&v1alpha1.Repository{}, nil)

		return mockKubectl, db, settingsManager, &mockRepoClientset
	}
	t.Run("success on GenerateManifest", func(t *testing.T) {
		mockKubectl, db, settingsManager, mockRepoClientset := setupMocks()
		mockRepoClientset.RepoServerServiceClient.(*mockrepoclient.RepoServerServiceClient).On("GenerateManifest", mock.Anything, mock.Anything).Return(&apiclient.ManifestResponse{}, nil)
		repoServerManager := &repoServerManager{
			db:            db,
			settingsMgr:   settingsManager,
			repoClientset: mockRepoClientset,
			kubectl:       mockKubectl,
		}
		_, _, err := getApplicationChildManifests(ctx, &v1alpha1.Application{
			Spec: v1alpha1.ApplicationSpec{
				Destination: v1alpha1.ApplicationDestination{
					Server: "test",
				},
			},
		}, &v1alpha1.AppProject{}, "test-argocd", repoServerManager)
		assert.NoError(t, err)
	})
	t.Run("failure on GenerateManifest", func(t *testing.T) {
		mockKubectl, db, settingsManager, mockRepoClientset := setupMocks()
		mockRepoClientset.RepoServerServiceClient.(*mockrepoclient.RepoServerServiceClient).On("GenerateManifest", mock.Anything, mock.Anything).Return(&apiclient.ManifestResponse{}, fmt.Errorf("error"))
		repoServerManager := &repoServerManager{
			db:            db,
			settingsMgr:   settingsManager,
			repoClientset: mockRepoClientset,
			kubectl:       mockKubectl,
		}
		_, _, err := getApplicationChildManifests(ctx, &v1alpha1.Application{
			Spec: v1alpha1.ApplicationSpec{
				Destination: v1alpha1.ApplicationDestination{
					Server: "test",
				},
			},
		}, &v1alpha1.AppProject{}, "test-argocd", repoServerManager)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "error")
	})
}

func TestUnmarshalManifests(t *testing.T) {
	validManifest := `{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"labels":{"app":"example"},"name":"example-sts"},"spec":{"replicas":2,"selector":{"matchLabels":{"app":"example"}},"serviceName":"example-service","template":{"metadata":{"labels":{"app":"example"}},"spec":{"containers":[{"image":"nginx:latest","name":"nginx","ports":[{"containerPort":80}],"volumeMounts":[{"mountPath":"/usr/share/nginx/html","name":"example-pvc"}]}]}},"volumeClaimTemplates":[{"metadata":{"name":"example-pvc"},"spec":{"accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"1Gi"}},"storageClassName":"standard"}}]}}`
	invalidManifest := `{"apiVersion":"apps/v1","kind":"InvalidKind","metadata":{"name":}`
	t.Run("valid manifest", func(t *testing.T) {
		manifests := []string{validManifest}
		objs, err := unmarshalManifests(manifests)
		assert.NoError(t, err)
		assert.Len(t, objs, 1)
		assert.Equal(t, "StatefulSet", objs[0].GetKind())
		assert.Equal(t, "example-sts", objs[0].GetName())
	})
	t.Run("invalid manifest", func(t *testing.T) {
		manifests := []string{invalidManifest}
		objs, err := unmarshalManifests(manifests)
		assert.Error(t, err)
		assert.Nil(t, objs)
	})
}

func TestGetDestinationServer(t *testing.T) {
	ctx := context.Background()
	t.Run("single cluster found", func(t *testing.T) {
		mockDB := &dbmocks.ArgoDB{}
		mockDB.On("GetClusterServersByName", ctx, "test-cluster").Return([]string{"https://test-cluster-server"}, nil)
		server, err := getDestinationServer(ctx, mockDB, "test-cluster")
		assert.NoError(t, err)
		assert.Equal(t, "https://test-cluster-server", server)
	})
	t.Run("multiple clusters found", func(t *testing.T) {
		mockDB := &dbmocks.ArgoDB{}
		mockDB.On("GetClusterServersByName", ctx, "test-cluster").Return([]string{"https://test-cluster-server1", "https://test-cluster-server2"}, nil)
		server, err := getDestinationServer(ctx, mockDB, "test-cluster")
		assert.Error(t, err)
		assert.EqualError(t, err, "there are 2 clusters with the same name: [https://test-cluster-server1 https://test-cluster-server2]")
		assert.Empty(t, server)
	})
	t.Run("no clusters found", func(t *testing.T) {
		mockDB := &dbmocks.ArgoDB{}
		mockDB.On("GetClusterServersByName", ctx, "test-cluster").Return([]string{}, nil)
		server, err := getDestinationServer(ctx, mockDB, "test-cluster")
		assert.Error(t, err)
		assert.EqualError(t, err, "there are no clusters with this name: test-cluster")
		assert.Empty(t, server)
	})
	t.Run("error fetching clusters", func(t *testing.T) {
		mockDB := &dbmocks.ArgoDB{}
		mockDB.On("GetClusterServersByName", ctx, "test-cluster").Return(nil, fmt.Errorf("database error"))
		server, err := getDestinationServer(ctx, mockDB, "test-cluster")
		assert.Error(t, err)
		assert.EqualError(t, err, "error getting cluster server by name \"test-cluster\": database error")
		assert.Empty(t, server)
	})
}

func TestGetClusterAPIDetails(t *testing.T) {
	t.Run("Success Case", func(t *testing.T) {
		mockKubectl := &MockKubectl{Kubectl: &kubetest.MockKubectlCmd{}}
		config := &rest.Config{}
		mockKubectl.On("GetServerVersion", config).Return("v1.25.0", nil)
		mockKubectl.On("GetAPIResources", config, false, &settings.ResourcesFilter{}).Return(
			[]kube.APIResourceInfo{
				{GroupKind: schema.GroupKind{Group: "", Kind: "Pod"}},
			}, nil)

		details, err := getClusterAPIDetails(config, mockKubectl)
		assert.NoError(t, err)
		assert.NotNil(t, details)
		assert.Equal(t, "v1.25.0", details.APIVersions)
		assert.Equal(t, []kube.APIResourceInfo{
			{GroupKind: schema.GroupKind{Group: "", Kind: "Pod"}},
		}, details.APIResources)
	})
	t.Run("Failure in GetServerVersion", func(t *testing.T) {
		mockKubectl := &MockKubectl{Kubectl: &kubetest.MockKubectlCmd{}}
		config := &rest.Config{}
		mockKubectl.On("GetServerVersion", config).Return("", errors.New("server error"))
		details, err := getClusterAPIDetails(config, mockKubectl)
		assert.Error(t, err)
		assert.Nil(t, details)
		assert.Contains(t, err.Error(), "failed to get server version")
	})
	t.Run("Failure in GetAPIResources", func(t *testing.T) {
		mockKubectl := &MockKubectl{Kubectl: &kubetest.MockKubectlCmd{}}
		config := &rest.Config{}
		mockKubectl.On("GetServerVersion", config).Return("v1.25.0", nil)
		mockKubectl.On("GetAPIResources", config, false, &settings.ResourcesFilter{}).Return([]kube.APIResourceInfo{}, errors.New("resource error"))
		details, err := getClusterAPIDetails(config, mockKubectl)
		assert.Error(t, err)
		assert.Nil(t, details)
		assert.Contains(t, err.Error(), "failed to get API resources")
	})
}
