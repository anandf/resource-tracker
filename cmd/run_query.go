package main

import (
	"context"
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
	"gopkg.in/yaml.v3"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

type RunQueryConfig struct {
	applicationName      string
	applicationNamespace string
	trackingMethod       string
	globalQuery          *bool
	checkInterval        time.Duration
	logLevel             string
	once                 *bool
	kubeConfig           string
	argocdNamespace      string
	directUpdateEnabled  *bool
}

var (
	configMapGVR = schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "configmaps",
	}

	argoAppGVR = schema.GroupVersionResource{
		Group:    "argoproj.io",
		Version:  "v1alpha1",
		Resource: "applications",
	}

	crdGVR = schema.GroupVersionResource{
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
	}
)

var (
	queryServer          *graph.QueryServer
	dynamicClient        dynamic.Interface
	previousGroupedKinds graph.GroupedResourceKinds
	lastRunTime          time.Time
)

// newRunQueryCommand implements "runQuery" command which executes a cyphernetes graph query against a given kubeconfig
func newRunQueryCommand() *cobra.Command {
	cfg := &RunQueryConfig{}

	var runQueryCmd = &cobra.Command{
		Use:   "run-query",
		Short: "Runs the resource-tracker which executes a graph based query to fetch the dependencies",
		RunE: func(cmd *cobra.Command, args []string) error {
			if *cfg.once {
				cfg.checkInterval = 0
			}
			log.Infof("%s %s starting [loglevel:%s, interval:%s]",
				version.BinaryName(),
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
			return runQueryExecutorLoop(cfg)
		},
	}
	runQueryCmd.Flags().StringVar(&cfg.logLevel, "loglevel", env.GetStringVal("RESOURCE_TRACKER_LOGLEVEL", "info"), "set the loglevel to one of trace|debug|info|warn|error")
	runQueryCmd.Flags().StringVar(&cfg.kubeConfig, "kubeconfig", "", "full path to kube client configuration, i.e. ~/.kube/config")
	runQueryCmd.Flags().StringVar(&cfg.trackingMethod, "tracking-method", "label", "either label or annotation tracking used by Argo CD")
	runQueryCmd.Flags().StringVar(&cfg.applicationName, "app-name", "", "if only specific application resources needs to be tracked, by default all applications ")
	runQueryCmd.Flags().StringVar(&cfg.applicationNamespace, "app-namespace", "", "either label or annotation tracking used by Argo CD")
	runQueryCmd.Flags().StringVar(&cfg.argocdNamespace, "argocd-namespace", "argocd", "namespace where argocd control plane components are running")
	cfg.globalQuery = runQueryCmd.Flags().Bool("global", true, "perform graph query without listing applications and finding children for each application")
	cfg.once = runQueryCmd.Flags().Bool("once", false, "run the loop only once")
	cfg.directUpdateEnabled = runQueryCmd.Flags().Bool("direct-update", false, "if enabled updates the argocd-cm directly, else prints the output on screen")
	runQueryCmd.Flags().DurationVar(&cfg.checkInterval, "interval", 2*time.Minute, "interval for how often to check for updates")
	return runQueryCmd
}

// initQueryServer initializes the required kubernetes clients and the cyphernetes graph query executor.
// this is an expensive operation and done only once, and the same executor is used for further graph query executions
func initQueryServer(cfg *RunQueryConfig) error {
	// First try in-cluster config
	restConfig, err := rest.InClusterConfig()
	if err != nil && !errors.Is(err, rest.ErrNotInCluster) {
		return fmt.Errorf("failed to create config: %v", err)
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
			return fmt.Errorf("failed to create config: %v", err)
		}
	}
	queryServer, err = graph.NewQueryServer(restConfig, cfg.trackingMethod)
	if err != nil {
		return err
	}
	dynamicClient, err = dynamic.NewForConfig(restConfig)
	if err != nil {
		return err
	}
	return nil
}

// initArgoApplicationInformer initializes the shared informers for Argo CD Application objects.
// whenever a change to any Argo Application is detected, the graph query is executed and the resource inclusion
// entries are computed.
func initArgoApplicationInformer(cfg *RunQueryConfig) error {
	// Create a dynamic shared informer factory
	informerFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicClient, 1*time.Minute, "", nil)

	// Get the informer for the specified GVR
	informer := informerFactory.ForResource(argoAppGVR).Informer()

	// Add event handlers
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			unstructuredObj := obj.(*unstructured.Unstructured)
			log.Infof("Object Added: %s/%s\n", unstructuredObj.GetNamespace(), unstructuredObj.GetName())
			err := runQueryExecutor(cfg)
			if err != nil {
				log.Error(err)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldUnstructured := oldObj.(*unstructured.Unstructured)
			newUnstructured := newObj.(*unstructured.Unstructured)
			log.Infof("Object Updated: %s/%s (ResourceVersion: %s -> %s)\n",
				newUnstructured.GetNamespace(), newUnstructured.GetName(),
				oldUnstructured.GetResourceVersion(), newUnstructured.GetResourceVersion())
			err := runQueryExecutor(cfg)
			if err != nil {
				log.Error(err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			unstructuredObj := obj.(*unstructured.Unstructured)
			log.Infof("Object Deleted: %s/%s\n", unstructuredObj.GetNamespace(), unstructuredObj.GetName())
			err := runQueryExecutor(cfg)
			if err != nil {
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

// runQueryExecutorLoop runs the graph query continuously in a loop,
// executes the graph query and updates the argocd-cm configmap whenever there is a change detected in any Argo CD
// Application and check interval duration has passed.
func runQueryExecutorLoop(cfg *RunQueryConfig) error {
	err := initQueryServer(cfg)
	if err != nil {
		return err
	}
	if cfg.checkInterval == 0 {
		return runQueryExecutor(cfg)
	} else {
		err = initArgoApplicationInformer(cfg)
		if err != nil {
			return err
		}
		//err = initCRDInformer()
		//if err != nil {
		//	return err
		//}
	}
	return nil
}

// runQueryExecutor runs the graph query, computes the resources managed via Argo CD and update the resource.inclusions
// settings in the argocd-cm config map if it detects any new changes compared to the previous computed value or if its
// value is different from what is present in the argocd-cm config map.
// if the check interval time has not passed since the previous run, then the method returns without executing any queries.
func runQueryExecutor(cfg *RunQueryConfig) error {
	if !lastRunTime.IsZero() && time.Since(lastRunTime) < cfg.checkInterval {
		log.Info("skipping query executor due to last run not lapsed the check interval")
		return nil
	}
	log.Info("Starting query executor...")
	lastRunTime = time.Now()
	var argoAppResources []graph.ResourceInfo
	var allAppChildren []graph.ResourceInfo
	if *cfg.globalQuery {
		log.Infof("Querying Argo CD application globally for application '%s'...", cfg.applicationName)
		appChildren, err := queryServer.GetApplicationChildResources(cfg.applicationName, "")
		if err != nil {
			return err
		}
		log.Infof("Children of Argo CD application globally for application '%s': %v", cfg.applicationName, appChildren)
		for appChild := range appChildren {
			allAppChildren = append(allAppChildren, appChild)
		}
	} else {
		list, err := dynamicClient.Resource(argoAppGVR).Namespace(cfg.applicationNamespace).List(context.Background(), v1.ListOptions{})
		if err != nil {
			return err
		}
		for _, obj := range list.Items {
			if len(cfg.applicationName) > 0 {
				if cfg.applicationName == obj.GetName() {
					argoAppResources = append(argoAppResources, graph.ResourceInfo{Kind: obj.GetKind(), Name: obj.GetName(), Namespace: obj.GetNamespace()})
					break
				}
			} else {
				argoAppResources = append(argoAppResources, graph.ResourceInfo{Kind: obj.GetKind(), Name: obj.GetName(), Namespace: obj.GetNamespace()})
			}
		}
		for _, argoAppResource := range argoAppResources {
			log.Infof("Querying Argo CD application '%v'", argoAppResource)
			appChildren, err := queryServer.GetApplicationChildResources(argoAppResource.Name, "")
			if err != nil {
				return err
			}
			log.Infof("Children of Argo CD application '%s': %v", argoAppResource.Name, appChildren)
			for appChild := range appChildren {
				allAppChildren = append(allAppChildren, appChild)
			}
		}
	}
	groupedKinds := mergeResourceInfo(allAppChildren)
	if !isGroupedResourceKindsEqual(previousGroupedKinds, groupedKinds) {
		log.Infof("changes detected in resource inclusions, updating the argocd-cm configmap")
		err := updateResourceInclusion(cfg, &groupedKinds)
		if err != nil {
			return err
		}
	}
	previousGroupedKinds = groupedKinds
	return nil
}

// mergeResourceInfo merges all ResourceInfo objects according to their api groups
func mergeResourceInfo(input []graph.ResourceInfo) graph.GroupedResourceKinds {
	results := make(graph.GroupedResourceKinds)
	for _, resourceInfo := range input {
		if len(resourceInfo.APIVersion) <= 0 {
			continue
		}
		apiGroup := getAPIGroup(resourceInfo.APIVersion)
		if _, found := results[apiGroup]; !found {
			results[apiGroup] = map[string]graph.Void{
				resourceInfo.Kind: {},
			}
		} else {
			results[apiGroup][resourceInfo.Kind] = graph.Void{}
		}
	}
	return results
}

// getAPIGroup returns the API group for a given API version.
func getAPIGroup(apiVersion string) string {
	if strings.Contains(apiVersion, "/") {
		return strings.Split(apiVersion, "/")[0]
	}
	return ""
}

// getUniqueKinds given a set of kinds, it returns unique set of kinds
func getUniqueKinds(kinds graph.Kinds) []string {
	uniqueKinds := make([]string, 0)
	for kind := range kinds {
		uniqueKinds = append(uniqueKinds, kind)
	}
	return uniqueKinds
}

// isGroupedResourceKindsEqual returns true if any of the resource inclusions entries is modified, false otherwise
func isGroupedResourceKindsEqual(previous, current graph.GroupedResourceKinds) bool {
	if len(previous) != len(current) {
		return false
	}
	for groupName, previousKinds := range previous {
		if _, ok := current[groupName]; !ok {
			return false
		}
		currentKinds, _ := current[groupName]
		if !currentKinds.Equal(&previousKinds) {
			return false
		}
	}
	return true
}

// isResourceInclusionEntriesEqual validates if existing resource inclusion entries from argocd-cm and
// current computed resource inclusion entries are equal.
func isResourceInclusionEntriesEqual(existing, current []graph.ResourceInclusionEntry) bool {
	if len(existing) != len(current) {
		return false
	}
	for i := 0; i < len(existing); i++ {
		if !existing[i].Equal(&current[i]) {
			return false
		}
	}
	return true
}

// updateResourceInclusion updates the resource.inclusions and resource.exclusions settings in argocd-cm configmap
func updateResourceInclusion(cfg *RunQueryConfig, resourceInclusion *graph.GroupedResourceKinds) error {
	ctx := context.Background()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		argocdCM, err := dynamicClient.Resource(configMapGVR).Namespace(cfg.argocdNamespace).Get(ctx, "argocd-cm", v1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error fetching ConfigMap: %v", err)
		}

		includedResources := make([]graph.ResourceInclusionEntry, 0, len(*resourceInclusion))
		for group, kinds := range *resourceInclusion {
			includedResources = append(includedResources, graph.ResourceInclusionEntry{
				APIGroups: []string{group},
				Kinds:     getUniqueKinds(kinds),
				Clusters:  []string{"*"},
			})
		}
		out, err := yaml.Marshal(includedResources)
		if err != nil {
			return err
		}
		// include resources that are managed by Argo CD.
		resourceInclusionString := string(out)
		if !*cfg.directUpdateEnabled {
			log.Info("direct update or argocd-cm is disabled, printing the output on terminal")
			fmt.Printf("resource.inclusions: |\n%sresource.exclusions: ''\n", resourceInclusionString)
			return nil
		}
		existingResourceInclusionsInCMStr, found, err := unstructured.NestedString(argocdCM.Object, "data", "resource.inclusions")
		if err != nil {
			return err
		}
		if found {
			var existingResourceInclusionsInCM []graph.ResourceInclusionEntry
			err = yaml.Unmarshal([]byte(existingResourceInclusionsInCMStr), &existingResourceInclusionsInCM)
			if err == nil && isResourceInclusionEntriesEqual(existingResourceInclusionsInCM, includedResources) {
				log.Info("resource inclusions is already set to required value. skipping update")
				// exclude all resources that are not explicitly excluded.
				unstructured.RemoveNestedField(argocdCM.Object, "data", "resource.exclusions")
				return nil
			}
		}
		if err := unstructured.SetNestedField(argocdCM.Object, resourceInclusionString, "data", "resource.inclusions"); err != nil {
			return fmt.Errorf("failed to set resource.inclusions value: %v", err)
		}
		// exclude all resources that are not explicitly excluded.
		unstructured.RemoveNestedField(argocdCM.Object, "data", "resource.exclusions")

		// perform the actual update of the configmap
		_, err = dynamicClient.Resource(configMapGVR).Namespace(cfg.argocdNamespace).Update(ctx, argocdCM, v1.UpdateOptions{})
		if err != nil {
			log.Warningf("Retrying due to conflict: %v", err)
			return err
		}
		log.Infof("Resource inclusions updated successfully in argocd-cm ConfigMap.")
		return nil
	})
}

// initCRDInformer initializes the shared informers for Argo CD Application objects.
// whenever a change to any Argo Application is detected, the graph query is executed and the resource inclusion
// entries are computed.
func initCRDInformer() error {
	// Create a dynamic shared informer factory
	informerFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicClient, 1*time.Minute, "", nil)

	// Get the informer for the specified GVR
	informer := informerFactory.ForResource(crdGVR).Informer()

	// Add event handlers
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			unstructuredObj := obj.(*unstructured.Unstructured)
			log.Infof("Object Added: %s/%s\n", unstructuredObj.GetNamespace(), unstructuredObj.GetName())
			queryServer.AddRuleForResourceKind(unstructuredObj.GetKind())
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
	log.Info("Informer for CRD started successfully.")

	// Keep the main goroutine running until a signal is received
	<-sigCh
	log.Info("Received termination signal, stopping informer...")
	close(stopCh)
	return nil
}
