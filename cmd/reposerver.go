package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/anandf/resource-tracker/pkg/argocd"
	"github.com/anandf/resource-tracker/pkg/graph"
	"github.com/anandf/resource-tracker/pkg/kube"
	"github.com/argoproj/argo-cd/v2/common"
	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"

	"github.com/anandf/resource-tracker/pkg/env"
	"github.com/anandf/resource-tracker/pkg/version"
	"github.com/spf13/cobra"
)

// RepoServerCommandConfig contains global configuration and required runtime data
type RepoServerCommandConfig struct {
	argocdNamespace          string
	logLevel                 string
	repoServerAddress        string
	repoServerPlaintext      bool
	repoServerStrictTLS      bool
	repoServerTimeoutSeconds int
	kubeConfig               string
}

type RepoServerCommand struct {
	argoCDClient argocd.ArgoCD
	repoClient   *argocd.RepoServerManager
}

type manifestResponse struct {
	children          []*unstructured.Unstructured
	destinationConfig *rest.Config
	appName           string
}

func newRepoServerCommandController(cfg *RepoServerCommandConfig) (*RepoServerCommand, error) {
	// Prepare the KUBECONFIG to connect to the Kubernetes cluster.
	config, err := kube.GetKubeConfig(cfg.kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to get kube config: %w", err)
	}
	// Set this environment variable when running as CLI connecting to the Argo CD components running
	// inside a cluster.
	err = os.Setenv("ARGOCD_FAKE_IN_CLUSTER", "true")
	if err != nil {
		return nil, fmt.Errorf("failed to set env variable ARGOCD_FAKE_IN_CLUSTER: %w", err)
	}
	argoCD, err := argocd.NewArgoCD(config, cfg.argocdNamespace)
	if err != nil {
		return nil, err
	}
	repo, err := argocd.NewRepoServerManager(config, cfg.argocdNamespace, cfg.repoServerAddress, cfg.repoServerTimeoutSeconds,
		cfg.repoServerPlaintext, cfg.repoServerStrictTLS)
	if err != nil {
		return nil, err
	}
	return &RepoServerCommand{
		argoCDClient: argoCD,
		repoClient:   repo,
	}, nil
}

// newRepoServerCommand implements "run" command
func newRepoServerCommand() *cobra.Command {
	cfg := &RepoServerCommandConfig{}
	var runCmd = &cobra.Command{
		Use:   "run",
		Short: "Runs the resource-tracker with a set of options",
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
			r, err := newRepoServerCommandController(cfg)
			if err != nil {
				return err
			}
			return r.execute()
		},
	}
	runCmd.Flags().StringVar(&cfg.repoServerAddress, "repo-server", env.GetStringVal("ARGOCD_REPO_SERVER", common.DefaultRepoServerAddr), "Repo server address.")
	runCmd.Flags().StringVar(&cfg.logLevel, "loglevel", env.GetStringVal("RESOURCE_TRACKER_LOGLEVEL", "info"), "set the loglevel to one of trace|debug|info|warn|error")
	runCmd.Flags().IntVar(&cfg.repoServerTimeoutSeconds, "repo-server-timeout-seconds", 60, "Repo server RPC call timeout seconds.")
	runCmd.Flags().BoolVar(&cfg.repoServerPlaintext, "repo-server-plaintext", false, "Disable TLS on connections to repo server, Default: false")
	runCmd.Flags().BoolVar(&cfg.repoServerStrictTLS, "repo-server-strict-tls", false, "Whether to use strict validation of the TLS cert presented by the repo server, Default: false")
	runCmd.Flags().StringVar(&cfg.kubeConfig, "kubeconfig", "", "full path to kube client configuration, i.e. ~/.kube/config")
	runCmd.Flags().StringVar(&cfg.argocdNamespace, "argocd-namespace", "", "namespace where ArgoCD runs in (current namespace by default)")
	return runCmd
}

func (r *RepoServerCommand) execute() error {
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
	groupedKinds := make(graph.GroupedResourceKinds)
	groupedKinds.MergeResourceInfos(nestedResources)
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

	resourceInclusionString := groupedKinds.String()
	if strings.HasPrefix(resourceInclusionString, "error:") {
		return fmt.Errorf("error in yaml string of resource.inclusions: %s", resourceInclusionString)
	}
	fmt.Printf("resource.inclusions: |\n%sresource.exclusions: ''\n", resourceInclusionString)
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
