package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/anandf/resource-tracker/pkg/env"
	"github.com/anandf/resource-tracker/pkg/graph"
	"github.com/anandf/resource-tracker/pkg/version"
	"github.com/avitaltamir/cyphernetes/pkg/core"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

var applicationName string
var applicationNamespace string
var trackingMethod string

// newRunQueryCommand implements "runQuery" command which executes a cyphernetes graph query against a given kubeconfig
func newRunQueryCommand() *cobra.Command {
	cfg := &ResourceTrackerConfig{}
	var once bool
	var kubeConfig string

	var runQueryCmd = &cobra.Command{
		Use:   "run-query",
		Short: "Runs the resource-tracker which executes a graph based query to fetch the dependencies",
		RunE: func(cmd *cobra.Command, args []string) error {
			if once {
				cfg.CheckInterval = 0
			}
			log.Infof("%s %s starting [loglevel:%s, interval:%s]",
				version.BinaryName(),
				version.Version(),
				strings.ToUpper(cfg.LogLevel),
				cfg.CheckInterval,
			)
			var err error
			level, err := log.ParseLevel(cfg.LogLevel)
			if err != nil {
				return fmt.Errorf("failed to parse log level: %w", err)
			}
			log.SetLevel(level)
			core.LogLevel = cfg.LogLevel
			return runQueryExecutor()
		},
	}
	runQueryCmd.Flags().StringVar(&cfg.LogLevel, "loglevel", env.GetStringVal("RESOURCE_TRACKER_LOGLEVEL", "info"), "set the loglevel to one of trace|debug|info|warn|error")
	runQueryCmd.Flags().StringVar(&kubeConfig, "kubeconfig", "", "full path to kube client configuration, i.e. ~/.kube/config")
	runQueryCmd.Flags().StringVar(&trackingMethod, "tracking-method", "label", "either label or annotation tracking used by Argo CD")
	runQueryCmd.Flags().StringVar(&applicationName, "app-name", "", "if only specific application resources needs to be tracked, by default all applications ")
	runQueryCmd.Flags().StringVar(&applicationNamespace, "app-namespace", "", "either label or annotation tracking used by Argo CD")
	return runQueryCmd
}

func runQueryExecutor() error {
	log.Info("Starting query executor...")

	// First try in-cluster config
	restConfig, err := rest.InClusterConfig()
	if err != nil && !errors.Is(err, rest.ErrNotInCluster) {
		return fmt.Errorf("failed to create config: %v", err)
	}

	// If not running in-cluster, try loading from KUBECONFIG env or $HOME/.kube/config file
	if restConfig == nil {
		// Fall back to kubeconfig
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		configOverrides := &clientcmd.ConfigOverrides{}
		kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
		restConfig, err = kubeConfig.ClientConfig()
		if err != nil {
			return fmt.Errorf("failed to create config: %v", err)
		}
	}
	queryServer, err := graph.NewQueryServer(restConfig, trackingMethod)
	if err != nil {
		return err
	}
	appChildren, err := queryServer.GetApplicationChildResources(applicationName, applicationNamespace)
	if err != nil {
		return err
	}
	resourceInclusion := mergeResourceInfo(appChildren)
	includedResources := make([]graph.ResourceInclusionEntry, 0, len(resourceInclusion))
	for group, kinds := range resourceInclusion {
		includedResources = append(includedResources, graph.ResourceInclusionEntry{
			APIGroups: []string{group},
			Kinds:     kinds,
			Clusters:  []string{"*"},
		})
	}
	out, err := yaml.Marshal(includedResources)
	if err != nil {
		return err
	}
	os.Stdout.Write(out)

	return nil
}

func mergeResourceInfo(input graph.ResourceInfoSet) graph.GroupedResourceKinds {
	results := make(graph.GroupedResourceKinds, 0)

	for resourceInfo, _ := range input {
		if len(resourceInfo.APIVersion) <= 0 {
			continue
		}
		apiGroup := getAPIGroup(resourceInfo.APIVersion)
		kinds, ok := results[apiGroup]
		if !ok {
			results[apiGroup] = []string{resourceInfo.Kind}
		} else {
			results[apiGroup] = append(kinds, resourceInfo.Kind)
		}
	}
	return results
}

func getAPIGroup(apiVersion string) string {
	if strings.Contains(apiVersion, "/") {
		return strings.Split(apiVersion, "/")[0]
	}
	return ""
}
