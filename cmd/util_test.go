package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetKubeConfig(t *testing.T) {
	tests := []struct {
		name        string
		namespace   string
		configPath  string
		expectError bool
		expectedNS  string
	}{
		{
			name:        "Valid KubeConfig",
			namespace:   "",
			configPath:  "../test/testdata/kubernetes/config",
			expectError: false,
			expectedNS:  "default",
		},
		{
			name:        "Invalid KubeConfig Path",
			namespace:   "",
			configPath:  "invalid/kubernetes/config",
			expectError: true,
		},
		{
			name:        "Valid KubeConfig with Namespace",
			namespace:   "argocd",
			configPath:  "../test/testdata/kubernetes/config",
			expectError: false,
			expectedNS:  "argocd",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := getKubeConfig(context.TODO(), tt.namespace, tt.configPath)
			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.NotNil(t, client.KubeClient)
				assert.Equal(t, tt.expectedNS, client.KubeClient.Namespace)
			}
		})
	}
}
