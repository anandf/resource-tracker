package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/anandf/resource-tracker/pkg/argocd"
	"github.com/anandf/resource-tracker/pkg/env"
	"github.com/anandf/resource-tracker/pkg/graph"
	"github.com/anandf/resource-tracker/pkg/kube"
	"github.com/anandf/resource-tracker/pkg/version"
	"github.com/argoproj/argo-cd/v2/common"
	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/avitaltamir/cyphernetes/pkg/core"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

type RepoServerController struct {
	dynamicClient        dynamic.Interface
	restConfig           *rest.Config
	previousGroupedKinds graph.GroupedResourceKinds
	allAppChildren       interface{}
	queryServers         map[string]*graph.QueryServer
	argoCDClient         argocd.ArgoCD
	repoClient           *argocd.RepoServerManager
}

type RepoServerControllerConfig struct {
	checkInterval            time.Duration
	lastRunTime              time.Time
	logLevel                 string
	kubeConfig               string
	trackingMethod           string
	argocdNamespace          string
	updateEnabled            *bool
	updateResourceName       string
	updateResourceKind       string
	repoServerAddress        string
	repoServerPlaintext      bool
	repoServerStrictTLS      bool
	repoServerTimeoutSeconds int
}

type manifestResponse struct {
	children          []*unstructured.Unstructured
	destinationConfig *rest.Config
	appName           string
}

// newRunCommand implements "runQuery" command which executes a cyphernetes graph query against a given kubeconfig
func newRunCommand() *cobra.Command {
	cfg := &RepoServerControllerConfig{}
	var runCmd = &cobra.Command{
		Use:   "run",
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
			controller, err := newRepoServerResourceController(cfg)
			if err != nil {
				return err
			}
			return controller.initArgoApplicationRepoServerInformer(cfg)
		},
	}
	runCmd.Flags().StringVar(&cfg.logLevel, "loglevel", env.GetStringVal("RESOURCE_TRACKER_LOGLEVEL", "info"), "set the loglevel to one of trace|debug|info|warn|error")
	runCmd.Flags().StringVar(&cfg.kubeConfig, "kubeconfig", "", "full path to kube client configuration, i.e. ~/.kube/config")
	runCmd.Flags().StringVar(&cfg.trackingMethod, "tracking-method", graph.TrackingMethodLabel, "either label or annotation tracking used by Argo CD")
	runCmd.Flags().StringVar(&cfg.argocdNamespace, "argocd-namespace", "argocd", "namespace where argocd control plane components are running")
	cfg.updateEnabled = runCmd.Flags().Bool("update-enabled", false, "if enabled updates the argocd-cm directly, else prints the output on screen")
	runCmd.Flags().StringVar(&cfg.updateResourceName, "update-resource-name", "argocd-cm", "name of the resource that needs to be updated. Default: argocd-cm")
	runCmd.Flags().StringVar(&cfg.updateResourceKind, "update-resource-kind", "ConfigMap", "kind of resource that needs to be updated, "+
		"users can choose to update either spec.data in argocd-cm or spec.extraConfigs in ArgoCD resource, Default: ConfigMap")
	runCmd.Flags().DurationVar(&cfg.checkInterval, "interval", DefaultCheckInterval, "interval for how often to check for updates, "+
		"to avoid frequent execution of compute and memory intensive graph queries")
	runCmd.Flags().StringVar(&cfg.repoServerAddress, "repo-server", env.GetStringVal("ARGOCD_REPO_SERVER", common.DefaultRepoServerAddr), "Repo server address.")
	runCmd.Flags().IntVar(&cfg.repoServerTimeoutSeconds, "repo-server-timeout-seconds", 60, "Repo server RPC call timeout seconds.")
	runCmd.Flags().BoolVar(&cfg.repoServerPlaintext, "repo-server-plaintext", false, "Disable TLS on connections to repo server, Default: false")
	runCmd.Flags().BoolVar(&cfg.repoServerStrictTLS, "repo-server-strict-tls", false, "Whether to use strict validation of the TLS cert presented by the repo server, Default: false")

	return runCmd
}

func newRepoServerResourceController(cfg *RepoServerControllerConfig) (*RepoServerController, error) {
	if cfg.updateResourceKind != ConfigMapResourceKind && cfg.updateResourceName != ArgoCDResourceKind {
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
		queryServer, err := graph.NewQueryServer(clusterConfig, cfg.trackingMethod, false)
		if err != nil {
			return nil, err
		}
		queryServerMap[clusterConfig.Host] = queryServer
	}
	config, err := kube.GetKubeConfig(cfg.kubeConfig)
	if err != nil {
		return nil, err
	}
	argoClient, err := argocd.NewArgoCD(config, cfg.argocdNamespace)
	if err != nil {
		return nil, err
	}
	repoClient, err := argocd.NewRepoServerManager(config, cfg.argocdNamespace, cfg.repoServerAddress, cfg.repoServerTimeoutSeconds,
		cfg.repoServerPlaintext, cfg.repoServerStrictTLS)
	if err != nil {
		return nil, err
	}
	return &RepoServerController{
		dynamicClient: dynamicClient,
		queryServers:  queryServerMap,
		restConfig:    restConfig,
		argoCDClient:  argoClient,
		repoClient:    repoClient,
	}, nil
}

// initArgoApplicationRepoServerInformer initializes the shared informers for Argo CD Application objects.
// whenever a change to any Argo Application is detected, the graph query is executed and the resource inclusion
// entries are computed.
func (r *RepoServerController) initArgoApplicationRepoServerInformer(cfg *RepoServerControllerConfig) error {
	// Create a dynamic shared informer factory
	informerFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(r.dynamicClient, 1*time.Minute, "", nil)

	// Get the informer for the specified GVR
	informer := informerFactory.ForResource(graph.ArgoAppGVR).Informer()

	// Add event handlers
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			unstructuredObj := obj.(*unstructured.Unstructured)
			log.Infof("Object Added: %s/%s", unstructuredObj.GetNamespace(), unstructuredObj.GetName())
			if err := r.runRepoServerExecutor(cfg); err != nil {
				log.Error(err)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldUnstructured := oldObj.(*unstructured.Unstructured)
			newUnstructured := newObj.(*unstructured.Unstructured)
			log.Infof("Object Updated: %s/%s (ResourceVersion: %s -> %s)",
				newUnstructured.GetNamespace(), newUnstructured.GetName(),
				oldUnstructured.GetResourceVersion(), newUnstructured.GetResourceVersion())
			if err := r.runRepoServerExecutor(cfg); err != nil {
				log.Error(err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			unstructuredObj := obj.(*unstructured.Unstructured)
			log.Infof("Object Deleted: %s/%s", unstructuredObj.GetNamespace(), unstructuredObj.GetName())
			if err := r.runRepoServerExecutor(cfg); err != nil {
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
func (r *RepoServerController) runRepoServerExecutor(cfg *RepoServerControllerConfig) error {
	if !cfg.lastRunTime.IsZero() && time.Since(cfg.lastRunTime) < cfg.checkInterval {
		log.Info("skipping query executor due to last run not lapsed the check interval")
		return nil
	}
	log.Info("Starting resource tracking process...")
	apps, err := r.argoCDClient.ListApplications()
	if err != nil {
		log.Fatalf("Error while listing applications: %v", err)
	}
	log.Infof("Fetched %d applications", len(apps))
	resourceChan := make(chan manifestResponse)
	errChan := make(chan error)
	allAppChildren := make([]manifestResponse, 0)
	// Launch consumer (Tracker)
	go startResourceTrackerConsumer(resourceChan, &allAppChildren)
	var wg sync.WaitGroup
	for _, app := range apps {
		wg.Add(1)
		go func(app v1alpha1.Application) {
			log.Infof("processing application: %s", app.Name)
			defer wg.Done()
			appProject, err := r.argoCDClient.GetAppProject(app)
			if err != nil {
				errChan <- err
				return
			}
			// Get target object from repo-server
			targetObjs, destinationConfig, err := r.repoClient.GetApplicationChildManifests(context.Background(), &app, appProject)
			if err != nil {
				errChan <- err
				return
			}
			resourceChan <- manifestResponse{
				children:          targetObjs,
				destinationConfig: destinationConfig,
				appName:           app.Name,
			}
			log.Infof("Fetched target manifests from repo-server for application: %s", app.Name)
		}(app)
	}
	// Handle processing errors
	go func() {
		for err := range errChan {
			log.Errorf("Error processing application: %v", err)
		}
	}()
	log.Info("Resource tracking initiated for all applications.")
	wg.Wait()
	close(resourceChan)
	log.Info("Resource channel has been closed. Resource Tracker process has completed successfully.")
	var nestedResources = make([]graph.ResourceInfo, 0)
	for _, appChild := range allAppChildren {
		appGroupedResources, err := r.argoCDClient.ProcessApplication(appChild.children, appChild.appName, appChild.destinationConfig)
		if err != nil {
			return fmt.Errorf("error processing application: %w", err)
		}
		nestedResources = append(nestedResources, appGroupedResources...)
	}
	groupedKinds := graph.MergeResourceInfo(nestedResources)
	missingResources, err := r.argoCDClient.GetAllMissingResources()
	if err != nil {
		return fmt.Errorf("error while fetching missing resources: %v", err)
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
			err = handleUpdateInArgoCDCR(r.dynamicClient, cfg.updateResourceName, cfg.argocdNamespace, groupedKinds)
			if err != nil {
				return err
			}
		} else {
			err = handleUpdateInCM(r.dynamicClient, cfg.argocdNamespace, groupedKinds)
			if err != nil {
				return err
			}
		}
	}
	r.previousGroupedKinds = groupedKinds
	return nil
}

func startResourceTrackerConsumer(resourceChan <-chan manifestResponse, allAppChildren *[]manifestResponse) {
	// Process resources from the channel
	// and update the tracked resources in the config
	// This is a blocking call, so it will keep running until the channel is closed
	for groupedResources := range resourceChan {
		*allAppChildren = append(*allAppChildren, groupedResources)
	}

}
