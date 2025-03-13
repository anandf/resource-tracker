package main

import (
	"context"

	"github.com/anandf/resource-tracker/pkg/kube"
)

func getKubeConfig(ctx context.Context, namespace string, kubeConfig string) (*kube.ResourceTrackerKubeClient, error) {

	kubeClient, err := kube.NewKubernetesClientFromConfig(ctx, namespace, kubeConfig)
	if err != nil {
		return nil, err
	}

	return kubeClient, nil
}
