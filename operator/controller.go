package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/anandf/resource-tracker/pkg/env"
	"github.com/anandf/resource-tracker/pkg/graph"
	"github.com/anandf/resource-tracker/pkg/version"
	"github.com/avitaltamir/cyphernetes/pkg/core"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

type ResourceController struct {
	dynamicClient        dynamic.Interface
	restConfig           *rest.Config
	previousGroupedKinds graph.GroupedResourceKinds
	allAppChildren       interface{}
	queryServers         map[string]*graph.QueryServer
}

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

const (
	DefaultCheckInterval  = 5 * time.Minute
	ConfigMapResourceKind = "ConfigMap"
	ArgoCDResourceKind    = "ArgoCD"
)

// newRunQueryCommand implements "runQuery" command which executes a cyphernetes graph query against a given kubeconfig
func newRunQueryCommand() *cobra.Command {
	cfg := &ResourceControllerConfig{}
	var runQueryCmd = &cobra.Command{
		Use:   "run-query",
		Short: "Runs the resource-tracker which executes a graph based query to fetch the dependencies",
		RunE: func(cmd *cobra.Command, args []string) error {
			log.Infof("%s %s starting [loglevel:%s, interval:%s]",
				fmt.Sprintf("%s-%s", version.BinaryName(), "operator"),
				version.Version(),
				strings.ToUpper(cfg.logLevel),
				cfg.checkInterval,
			)
			var err error
			level, err := log.ParseLevel(cfg.logLevel)
			if err != nil {
				return fmt.Errorf("failed to parse log level: %w", err)
			}
			log.SetLevel(level)
			core.LogLevel = cfg.logLevel
			controller, err := newResourceController(cfg)
			if err != nil {
				return err
			}
			return controller.initArgoApplicationInformer(cfg)
		},
	}
	runQueryCmd.Flags().StringVar(&cfg.logLevel, "loglevel", env.GetStringVal("RESOURCE_TRACKER_LOGLEVEL", "info"), "set the loglevel to one of trace|debug|info|warn|error")
	runQueryCmd.Flags().StringVar(&cfg.kubeConfig, "kubeconfig", "", "full path to kube client configuration, i.e. ~/.kube/config")
	runQueryCmd.Flags().StringVar(&cfg.trackingMethod, "tracking-method", "label", "either label or annotation tracking used by Argo CD")
	runQueryCmd.Flags().StringVar(&cfg.argocdNamespace, "argocd-namespace", "argocd", "namespace where argocd control plane components are running")
	cfg.updateEnabled = runQueryCmd.Flags().Bool("update-enabled", false, "if enabled updates the argocd-cm directly, else prints the output on screen")
	runQueryCmd.Flags().StringVar(&cfg.updateResourceName, "update-resource-name", "argocd-cm", "name of the resource that needs to be updated. Default: argocd-cm")
	runQueryCmd.Flags().StringVar(&cfg.updateResourceKind, "update-resource-kind", "ConfigMap", "kind of resource that needs to be updated, "+
		"users can choose to update either spec.data in argocd-cm or spec.extraConfigs in ArgoCD resource, Default: ConfigMap")
	runQueryCmd.Flags().DurationVar(&cfg.checkInterval, "interval", DefaultCheckInterval, "interval for how often to check for updates, "+
		"to avoid frequent execution of compute and memory intensive graph queries")
	return runQueryCmd
}

func newResourceController(cfg *ResourceControllerConfig) (*ResourceController, error) {
	if cfg.updateResourceKind == ConfigMapResourceKind || cfg.updateResourceName == ArgoCDResourceKind {
		return nil, fmt.Errorf("invalid update-resource-kind, valid values are ConfigMap and ArgoCD")
	}
	// First try in-cluster config
	restConfig, err := rest.InClusterConfig()
	if err != nil && !errors.Is(err, rest.ErrNotInCluster) {
		return nil, fmt.Errorf("failed to create config: %v", err)
	}

	// If not running in-cluster, try loading from KUBECONFIG env or $HOME/.kube/config file
	if restConfig == nil {
		// Fall back to kubeconfig
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		loadingRules.ExplicitPath = cfg.kubeConfig
		configOverrides := &clientcmd.ConfigOverrides{}
		kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
		restConfig, err = kubeConfig.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to create config: %v", err)
		}
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

	for _, clusterConfig := range clusterConfigs {
		if len(clusterConfig.Host) == 0 {
			continue
		}
		queryServer, err := graph.NewQueryServer(clusterConfig, cfg.trackingMethod)
		if err != nil {
			return nil, err
		}
		queryServerMap[clusterConfig.Host] = queryServer
	}
	return &ResourceController{
		dynamicClient: dynamicClient,
		queryServers:  queryServerMap,
		restConfig:    restConfig,
	}, nil
}

// initArgoApplicationInformer initializes the shared informers for Argo CD Application objects.
// whenever a change to any Argo Application is detected, the graph query is executed and the resource inclusion
// entries are computed.
func (r *ResourceController) initArgoApplicationInformer(cfg *ResourceControllerConfig) error {
	// Create a dynamic shared informer factory
	informerFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(r.dynamicClient, 1*time.Minute, "", nil)

	// Get the informer for the specified GVR
	informer := informerFactory.ForResource(graph.ArgoAppGVR).Informer()

	// Add event handlers
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			unstructuredObj := obj.(*unstructured.Unstructured)
			log.Infof("Object Added: %s/%s", unstructuredObj.GetNamespace(), unstructuredObj.GetName())
			if err := r.runQueryExecutor(cfg); err != nil {
				log.Error(err)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldUnstructured := oldObj.(*unstructured.Unstructured)
			newUnstructured := newObj.(*unstructured.Unstructured)
			log.Infof("Object Updated: %s/%s (ResourceVersion: %s -> %s)",
				newUnstructured.GetNamespace(), newUnstructured.GetName(),
				oldUnstructured.GetResourceVersion(), newUnstructured.GetResourceVersion())
			if err := r.runQueryExecutor(cfg); err != nil {
				log.Error(err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			unstructuredObj := obj.(*unstructured.Unstructured)
			log.Infof("Object Deleted: %s/%s", unstructuredObj.GetNamespace(), unstructuredObj.GetName())
			if err := r.runQueryExecutor(cfg); err != nil {
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

// runQueryExecutor runs the graph query, computes the resources managed via Argo CD and update the resource.inclusions
// settings in the argocd-cm config map if it detects any new changes compared to the previous computed value or if its
// value is different from what is present in the argocd-cm config map.
// if the check interval time has not passed since the previous run, then the method returns without executing any queries.
func (r *ResourceController) runQueryExecutor(cfg *ResourceControllerConfig) error {
	if !cfg.lastRunTime.IsZero() && time.Since(cfg.lastRunTime) < cfg.checkInterval {
		log.Info("skipping query executor due to last run not lapsed the check interval")
		return nil
	}
	var allAppChildren []graph.ResourceInfo
	cfg.lastRunTime = time.Now()
	for host, qs := range r.queryServers {
		log.Infof("Querying Argo CD application globally for application in host %s", host)
		qs.VisitedKinds = make(map[graph.ResourceInfo]bool)
		appChildren, err := qs.GetApplicationChildResources("", "")
		if err != nil {
			return err
		}
		log.Infof("Children of Argo CD application globally for application: %v", appChildren)
		for appChild := range appChildren {
			allAppChildren = append(allAppChildren, appChild)
		}
	}

	groupedKinds := graph.MergeResourceInfo(allAppChildren)
	missingResources, err := graph.GetAllMissingResources(r.dynamicClient, cfg.argocdNamespace)
	if err != nil {
		return err
	}
	// Check if additional resources are missing, if so add it.
	for _, resource := range missingResources {
		log.Infof("adding missing resource '%v'", resource)
		if kindMap, ok := groupedKinds[resource.APIVersion]; !ok && kindMap == nil {
			groupedKinds[resource.APIVersion] = graph.Kinds{resource.Kind: graph.Void{}}
		} else {
			groupedKinds[resource.APIVersion][resource.Kind] = graph.Void{}
		}
	}
	if !*cfg.updateEnabled {
		if !graph.IsGroupedResourceKindsEqual(r.previousGroupedKinds, groupedKinds) {
			log.Info("direct update or argocd-cm is disabled, printing the output on terminal")
			resourceInclusionString, err := graph.GetResourceInclusionsString(&groupedKinds)
			if err != nil {
				return err
			}
			fmt.Printf("resource.inclusions: |\n%sresource.exclusions: ''\n", resourceInclusionString)
		} else {
			log.Infof("no changes detected in previously computed resource inclusions and current computed resource inclusions")
		}
	} else {
		if cfg.updateResourceKind == ArgoCDResourceKind {
			r.handUpdateInArgoCDCR(cfg, groupedKinds)
		} else {
			r.handUpdateInCM(cfg, groupedKinds)
		}
	}
	r.previousGroupedKinds = groupedKinds
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
		if len(kubeConfig.Host) == 0 {
			log.Errorf("ignoring kubeconfig with empty host for cluster secret '%s/%s'", secret.GetNamespace(), secret.GetName())
			continue
		}
		kubeConfigs = append(kubeConfigs, &kubeConfig)
	}
	return kubeConfigs, nil
}

// handUpdateInCM handles the update of resource.inclusions settings in argocd-cm ConfigMap
func (r *ResourceController) handUpdateInCM(cfg *ResourceControllerConfig, groupedKinds graph.GroupedResourceKinds) error {
	existingGroupKinds, err := graph.GetCurrentGroupedKindsFromCM(r.dynamicClient, cfg.argocdNamespace)
	if err != nil {
		return err
	}
	if !graph.IsGroupedResourceKindsEqual(existingGroupKinds, groupedKinds) {
		log.Infof("changes detected in resource inclusions, updating the argocd-cm configmap")
		err = graph.UpdateResourceInclusion(r.dynamicClient, cfg.argocdNamespace, &groupedKinds)
		if err != nil {
			return err
		}
	} else {
		log.Info("no changes detected in existing resource inclusions in argocd-cm configmap")
	}
	return nil
}

// handUpdateInArgoCDCR handles the update of resource.inclusions settings in ArgoCD CustomResource
func (r *ResourceController) handUpdateInArgoCDCR(cfg *ResourceControllerConfig, groupedKinds graph.GroupedResourceKinds) error {
	existingGroupKinds, err := graph.GetCurrentGroupedKindsFromArgoCDCR(r.dynamicClient, cfg.updateResourceName, cfg.argocdNamespace)
	if err != nil {
		return err
	}
	if !graph.IsGroupedResourceKindsEqual(existingGroupKinds, groupedKinds) {
		log.Infof("changes detected in resource inclusions, updating the argocd-cm configmap")
		err = graph.UpdateResourceInclusionInArgoCDCR(r.dynamicClient, cfg.updateResourceName, cfg.argocdNamespace, &groupedKinds)
		if err != nil {
			return err
		}
	} else {
		log.Info("no changes detected in existing resource inclusions in argocd-cm configmap")
	}
	return nil
}
