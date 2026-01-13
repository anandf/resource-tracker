package kube

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
)

func Test_NewKubernetesClientFromConfig(t *testing.T) {
	t.Run("Get new K8s client for remote cluster instance", func(t *testing.T) {
		config, err := GetKubeConfig("../../test/testdata/kubernetes/config")
		require.NoError(t, err)
		client, err := NewKubernetesClientFromConfig(context.TODO(), "", config)
		require.NoError(t, err)
		assert.NotNil(t, client)
		assert.Equal(t, "default", client.KubeClient.Namespace)
	})

	t.Run("Get new K8s client for remote cluster instance specified namespace", func(t *testing.T) {
		config, err := GetKubeConfig("../../test/testdata/kubernetes/config")
		require.NoError(t, err)
		client, err := NewKubernetesClientFromConfig(context.TODO(), "argocd", config)
		require.NoError(t, err)
		assert.NotNil(t, client)
		assert.Equal(t, "argocd", client.KubeClient.Namespace)
	})
}
func Test_NewKubernetesClient(t *testing.T) {
	t.Run("Create new Kubernetes client with valid inputs", func(t *testing.T) {
		mockClientset := &kubernetes.Clientset{} // Mock clientset
		namespace := "test-namespace"
		ctx := context.TODO()

		client := NewKubernetesClient(ctx, mockClientset, namespace)

		assert.NotNil(t, client)
		assert.Equal(t, ctx, client.Context)
		assert.Equal(t, mockClientset, client.Clientset)
		assert.Equal(t, namespace, client.Namespace)
	})

	t.Run("Create new Kubernetes client with empty namespace", func(t *testing.T) {
		mockClientset := &kubernetes.Clientset{} // Mock clientset
		namespace := ""
		ctx := context.TODO()

		client := NewKubernetesClient(ctx, mockClientset, namespace)

		assert.NotNil(t, client)
		assert.Equal(t, ctx, client.Context)
		assert.Equal(t, mockClientset, client.Clientset)
		assert.Equal(t, namespace, client.Namespace)
	})
}
