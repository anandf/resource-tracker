package main

import (
	"errors"
	"fmt"

	"strings"

	"github.com/anandf/resource-tracker/pkg/argocd"
	"github.com/anandf/resource-tracker/pkg/env"
	"github.com/anandf/resource-tracker/pkg/graph"
	"github.com/anandf/resource-tracker/pkg/version"
	"github.com/avitaltamir/cyphernetes/pkg/core"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type GraphQueryConfig struct {
	applicationName      string
	applicationNamespace string
	globalQuery          *bool
	logLevel             string
	kubeConfig           string
	argocdNamespace      string
}

type GraphQueryCommand struct {
	queryServer  *graph.QueryServer
	argoCDClient argocd.ArgoCD
}

// newGraphQueryCommand implements "runQuery" command which executes a cyphernetes graph query against a given kubeconfig
func newGraphQueryCommand() *cobra.Command {
	cfg := &GraphQueryConfig{}

	var runQueryCmd = &cobra.Command{
		Use:   "run-query",
		Short: "Runs the resource-tracker which executes a graph based query to fetch the dependencies",
		RunE: func(cmd *cobra.Command, args []string) error {
			log.Infof("%s %s starting [loglevel:%s]",
				version.BinaryName(),
				version.Version(),
				strings.ToUpper(cfg.logLevel),
			)
			var err error
			level, err := log.ParseLevel(cfg.logLevel)
			if err != nil {
				return fmt.Errorf("failed to parse log level: %w", err)
			}
			log.SetLevel(level)
			core.LogLevel = cfg.logLevel
			r, err := newGraphQueryCommandController(cfg)
			if err != nil {
				return err
			}
			if err != nil {
				return err
			}
			return r.execute(cfg)
		},
	}
	runQueryCmd.Flags().StringVar(&cfg.logLevel, "loglevel", env.GetStringVal("RESOURCE_TRACKER_LOGLEVEL", "info"), "set the loglevel to one of trace|debug|info|warn|error")
	runQueryCmd.Flags().StringVar(&cfg.kubeConfig, "kubeconfig", "", "full path to kube client configuration, i.e. ~/.kube/config")
	runQueryCmd.Flags().StringVar(&cfg.applicationName, "app-name", "", "if only specific application resources needs to be tracked, by default all applications ")
	runQueryCmd.Flags().StringVar(&cfg.applicationNamespace, "app-namespace", "", "namespace for the given application. Default value is empty string indicating cluster scope")
	runQueryCmd.Flags().StringVar(&cfg.argocdNamespace, "argocd-namespace", "argocd", "namespace where argocd control plane components are running")
	cfg.globalQuery = runQueryCmd.Flags().Bool("global", true, "perform graph query without listing applications and finding children for each application")
	return runQueryCmd
}

// newGraphQueryCommandController initializes the required kubernetes clients and the cyphernetes graph query executor.
// this is an expensive operation and done only once, and the same executor is used for further graph query executions
func newGraphQueryCommandController(cfg *GraphQueryConfig) (*GraphQueryCommand, error) {
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

	argoCD, err := argocd.NewArgoCD(restConfig, cfg.argocdNamespace)
	if err != nil {
		return nil, err
	}
	trackingMethod, err := argoCD.GetTrackingMethod()
	if err != nil {
		return nil, err
	}
	qs, err := graph.NewQueryServer(restConfig, trackingMethod, true)
	if err != nil {
		return nil, err
	}
	return &GraphQueryCommand{
		queryServer:  qs,
		argoCDClient: argoCD,
	}, nil
}

// execute runs the graph query, computes the resources managed via Argo CD and prints it on the terminal
func (r *GraphQueryCommand) execute(cfg *GraphQueryConfig) error {
	log.Info("Starting query executor...")
	var allAppChildren []graph.ResourceInfo
	if *cfg.globalQuery {
		log.Infof("Querying Argo CD application globally for application '%s'...", cfg.applicationName)
		appChildren, err := r.queryServer.GetApplicationChildResources(cfg.applicationName, "")
		if err != nil {
			return err
		}
		log.Infof("Children of Argo CD application globally for application '%s': %s", cfg.applicationName, appChildren.String())
		for appChild := range appChildren {
			allAppChildren = append(allAppChildren, appChild)
		}
	} else {
		argoAppResources, err := r.argoCDClient.ListApplications()
		if err != nil {
			return err
		}
		for _, argoAppResource := range argoAppResources {
			log.Infof("Querying Argo CD application '%v'", argoAppResource)
			appChildren, err := r.queryServer.GetApplicationChildResources(argoAppResource.Name, "")
			if err != nil {
				return err
			}
			log.Infof("Children of Argo CD application '%s': %v", argoAppResource.Name, appChildren)
			for appChild := range appChildren {
				allAppChildren = append(allAppChildren, appChild)
			}
		}
	}
	groupedKinds := make(graph.GroupedResourceKinds)
	groupedKinds.MergeResourceInfos(allAppChildren)
	missingResources, err := r.argoCDClient.GetAllMissingResources()
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

	resourceInclusionString := groupedKinds.String()
	if strings.HasPrefix(resourceInclusionString, "error:") {
		return fmt.Errorf("error in yaml string of resource.inclusions: %s", resourceInclusionString)
	}
	fmt.Printf("resource.inclusions: |\n%sresource.exclusions: ''\n", resourceInclusionString)
	return nil
}
