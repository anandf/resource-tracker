package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/anandf/resource-tracker/pkg/graph"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

const (
	DefaultCheckInterval  = 5 * time.Minute
	ConfigMapResourceKind = "ConfigMap"
	ArgoCDResourceKind    = "ArgoCD"
)

type ResourceControllerConfig struct {
	checkInterval      time.Duration
	lastRunTime        time.Time
	logLevel           string
	kubeConfig         string
	trackingMethod     string
	argocdNamespace    string
	updateEnabled      *bool
	updateResourceName string
	updateResourceKind string
}

// Reads all the cluster credential secrets in the cluster and returns the kubeconfig instance
func listClusterConfigs(dynamicClient dynamic.Interface, argocdNS string) ([]*rest.Config, error) {
	log.Info("Listing Argo CD cluster secrets")
	secrets, err := dynamicClient.Resource(graph.SecretGVR).Namespace(argocdNS).List(context.Background(), metav1.ListOptions{
		LabelSelector: "argocd.argoproj.io/secret-type=cluster",
	})
	if err != nil {
		return nil, fmt.Errorf("error listing cluster secrets %w", err)
	}
	kubeConfigs := make([]*rest.Config, 0, len(secrets.Items))
	for _, secret := range secrets.Items {
		if managedBy, ok := secret.GetAnnotations()["managed-by"]; !ok || managedBy != "argocd.argoproj.io" {
			log.Warnf("skipping secret '%s/%s' as the required managed-by annotation not found", secret.GetNamespace(), secret.GetName())
			continue
		}
		clusterConfig, found, err := unstructured.NestedString(secret.Object, "data", "config")
		if err != nil {
			return nil, fmt.Errorf("error getting data.config from cluster secret %w", err)
		}
		if !found {
			continue
		}
		decodedConfig, err := base64.StdEncoding.DecodeString(clusterConfig)
		if err != nil {
			return nil, fmt.Errorf("error decoding cluster config from cluster secret %w", err)
		}
		kubeConfig := rest.Config{}
		err = json.Unmarshal(decodedConfig, &kubeConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal cluster config: %w", err)
		}
		if len(kubeConfig.Host) == 0 {
			log.Errorf("ignoring kubeconfig with empty host for cluster secret '%s/%s'", secret.GetNamespace(), secret.GetName())
			continue
		}
		kubeConfigs = append(kubeConfigs, &kubeConfig)
	}
	return kubeConfigs, nil
}

// handleUpdateInArgoCDCR handles the update of resource.inclusions settings in ArgoCD CustomResource
func handleUpdateInArgoCDCR(dynamicClient dynamic.Interface, resourceName, resourceNamespace string, groupedKinds graph.GroupedResourceKinds) error {
	existingGroupKinds, err := graph.GetCurrentGroupedKindsFromArgoCDCR(dynamicClient, resourceName, resourceNamespace)
	if err != nil {
		return err
	}
	if !graph.IsGroupedResourceKindsEqual(existingGroupKinds, groupedKinds) {
		log.Infof("changes detected in resource inclusions, updating the argocd-cm configmap")
		err = graph.UpdateResourceInclusionInArgoCDCR(dynamicClient, resourceName, resourceNamespace, &groupedKinds)
		if err != nil {
			return err
		}
	} else {
		log.Info("no changes detected in existing resource inclusions in argocd-cm configmap")
	}
	return nil
}

// handleUpdateInCM handles the update of resource.inclusions settings in argocd-cm ConfigMap
func handleUpdateInCM(dynamicClient dynamic.Interface, resourceNamespace string, groupedKinds graph.GroupedResourceKinds) error {
	existingGroupKinds, err := graph.GetCurrentGroupedKindsFromCM(dynamicClient, resourceNamespace)
	if err != nil {
		return err
	}
	if !graph.IsGroupedResourceKindsEqual(existingGroupKinds, groupedKinds) {
		log.Infof("changes detected in resource inclusions, updating the argocd-cm configmap")
		err = graph.UpdateResourceInclusion(dynamicClient, resourceNamespace, &groupedKinds)
		if err != nil {
			return err
		}
	} else {
		log.Info("no changes detected in existing resource inclusions in argocd-cm configmap")
	}
	return nil
}
