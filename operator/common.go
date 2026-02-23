package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	argocdcommon "github.com/argoproj/argo-cd/v3/common"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/anandf/resource-tracker/pkg/argocd"
	"github.com/anandf/resource-tracker/pkg/common"
	"github.com/anandf/resource-tracker/pkg/graph"
	"github.com/anandf/resource-tracker/pkg/kube"
)

const (
	DefaultCheckInterval  = 5 * time.Minute
	ConfigMapResourceKind = "ConfigMap"
	ArgoCDResourceKind    = "ArgoCD"
)

type Executable interface {
	execute() error
}

type BaseControllerConfig struct {
	checkInterval      time.Duration
	logLevel           string
	kubeConfig         string
	argocdNamespace    string
	updateEnabled      *bool
	updateResourceName string
	updateResourceKind string
}

type BaseController struct {
	dynamicClient        dynamic.Interface
	previousGroupedKinds common.GroupedResourceKinds
	queryServers         map[string]*graph.QueryServer
	argoCDClient         argocd.ArgoCD
	lastRunTime          time.Time
}

func newBaseController(cfg *BaseControllerConfig) (*BaseController, error) {
	if cfg.updateResourceKind != ConfigMapResourceKind && cfg.updateResourceName != ArgoCDResourceKind {
		return nil, errors.New("invalid update-resource-kind, valid values are ConfigMap and ArgoCD")
	}
	restConfig, err := kube.GetKubeConfig(cfg.kubeConfig)
	if err != nil {
		return nil, err
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	queryServerMap := map[string]*graph.QueryServer{}
	clusterConfigs, err := listClusterConfigs(dynamicClient, cfg.argocdNamespace)
	if err != nil {
		return nil, err
	}
	clusterConfigs = append(clusterConfigs, restConfig)
	argoClient, err := argocd.NewArgoCD(
		restConfig,
		cfg.argocdNamespace,
		"",
		argocdcommon.DefaultRepoServerAddr,
		10,
		false,
		false,
	)
	if err != nil {
		return nil, err
	}
	trackingMethod, err := argoClient.GetTrackingMethod()
	if err != nil {
		return nil, err
	}
	for _, clusterConfig := range clusterConfigs {
		if clusterConfig.Host == "" {
			continue
		}
		queryServer, err := graph.NewQueryServer(clusterConfig, trackingMethod)
		if err != nil {
			return nil, err
		}
		queryServerMap[clusterConfig.Host] = queryServer
	}
	return &BaseController{
		dynamicClient: dynamicClient,
		queryServers:  queryServerMap,
		argoCDClient:  argoClient,
	}, nil
}

// initApplicationInformer initializes the shared informers for Argo CD Application objects.
// whenever a change to any Argo Application is detected, the graph query is executed and the resource inclusion
// entries are computed.
func initApplicationInformer(dynamicClient dynamic.Interface, executor Executable) error {
	// Create a dynamic shared informer factory
	informerFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicClient, 1*time.Minute, "", nil)

	// Get the informer for the specified GVR
	informer := informerFactory.ForResource(graph.ArgoAppGVR).Informer()

	// Add event handlers
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			unstructuredObj := obj.(*unstructured.Unstructured)
			log.Infof("Object Added: %s/%s", unstructuredObj.GetNamespace(), unstructuredObj.GetName())
			if err := executor.execute(); err != nil {
				log.Error(err)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldUnstructured := oldObj.(*unstructured.Unstructured)
			newUnstructured := newObj.(*unstructured.Unstructured)
			log.Infof("Object Updated: %s/%s (ResourceVersion: %s -> %s)",
				newUnstructured.GetNamespace(), newUnstructured.GetName(),
				oldUnstructured.GetResourceVersion(), newUnstructured.GetResourceVersion())
			if err := executor.execute(); err != nil {
				log.Error(err)
			}
		},
		DeleteFunc: func(obj any) {
			unstructuredObj := obj.(*unstructured.Unstructured)
			log.Infof("Object Deleted: %s/%s", unstructuredObj.GetNamespace(), unstructuredObj.GetName())
			if err := executor.execute(); err != nil {
				log.Error(err)
			}
		},
	})
	if err != nil {
		return err
	}

	// Set up a stop channel for graceful shutdown
	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start the informer factory
	informerFactory.Start(stopCh)

	// Wait for the informer's cache to sync
	if !cache.WaitForCacheSync(stopCh, informer.HasSynced) {
		log.Warn("Failed to sync informer cache")
		close(stopCh)
		return nil
	}
	log.Info("Informer for argo applications started successfully.")

	// Keep the main goroutine running until a signal is received
	<-sigCh
	log.Info("Received termination signal, stopping informer...")
	close(stopCh)
	return nil
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
		if kubeConfig.Host == "" {
			log.Errorf("ignoring kubeconfig with empty host for cluster secret '%s/%s'", secret.GetNamespace(), secret.GetName())
			continue
		}
		kubeConfigs = append(kubeConfigs, &kubeConfig)
	}
	return kubeConfigs, nil
}

// handleUpdateInArgoCDCR handles the update of resource.inclusions settings in ArgoCD CustomResource
func handleUpdateInArgoCDCR(argoCDClient argocd.ArgoCD, resourceName, resourceNamespace string, groupedKinds common.GroupedResourceKinds) error {
	currentResourceInclusions, err := argoCDClient.GetCurrentResourceInclusions(&graph.ArgoCDGVR, resourceName, resourceNamespace)
	if err != nil {
		return err
	}
	existingGroupKinds := make(common.GroupedResourceKinds)
	err = existingGroupKinds.FromYaml(currentResourceInclusions)
	if err != nil {
		return err
	}
	if !existingGroupKinds.Equal(&groupedKinds) {
		log.Infof("changes detected in resource inclusions, updating the argocd-cm configmap")
		err = argoCDClient.UpdateResourceInclusions(&graph.ArgoCDGVR, resourceName, resourceNamespace, groupedKinds.String())
		if err != nil {
			return err
		}
	} else {
		log.Info("no changes detected in existing resource inclusions in argocd-cm configmap")
	}
	return nil
}

// handleUpdateInCM handles the update of resource.inclusions settings in argocd-cm ConfigMap
func handleUpdateInCM(argoCDClient argocd.ArgoCD, resourceNamespace string, groupedKinds common.GroupedResourceKinds) error {
	currentResourceInclusions, err := argoCDClient.GetCurrentResourceInclusions(&graph.ConfigMapGVR, "argocd-cm", resourceNamespace)
	if err != nil {
		return err
	}
	existingGroupKinds := make(common.GroupedResourceKinds)
	err = existingGroupKinds.FromYaml(currentResourceInclusions)
	if err != nil {
		return err
	}
	if !existingGroupKinds.Equal(&groupedKinds) {
		log.Infof("changes detected in resource inclusions, updating the argocd-cm configmap")
		err = argoCDClient.UpdateResourceInclusions(&graph.ConfigMapGVR, "argocd-cm", resourceNamespace, groupedKinds.String())
		if err != nil {
			return err
		}
	} else {
		log.Info("no changes detected in existing resource inclusions in argocd-cm configmap")
	}
	return nil
}
