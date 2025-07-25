package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/anandf/resource-tracker/pkg/argocd"
	"github.com/anandf/resource-tracker/pkg/env"
	"github.com/anandf/resource-tracker/pkg/graph"
	"github.com/anandf/resource-tracker/pkg/version"
	"github.com/argoproj/argo-cd/v2/common"
	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/avitaltamir/cyphernetes/pkg/core"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

type RepoServerQueryController struct {
	*BaseController
	cfg        *RepoServerQueryControllerConfig
	repoClient *argocd.RepoServerManager
}

type RepoServerQueryControllerConfig struct {
	BaseControllerConfig
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
	cfg := &RepoServerQueryControllerConfig{}
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
			controller, err := newRepoServerQueryController(cfg)
			if err != nil {
				return err
			}
			return initApplicationInformer(controller.dynamicClient, controller)
		},
	}
	runCmd.Flags().StringVar(&cfg.logLevel, "loglevel", env.GetStringVal("RESOURCE_TRACKER_LOGLEVEL", "info"), "set the loglevel to one of trace|debug|info|warn|error")
	runCmd.Flags().StringVar(&cfg.kubeConfig, "kubeconfig", "", "full path to kube client configuration, i.e. ~/.kube/config")
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

func newRepoServerQueryController(cfg *RepoServerQueryControllerConfig) (*RepoServerQueryController, error) {
	base, err := newBaseController(&cfg.BaseControllerConfig)
	if err != nil {
		return nil, err
	}
	repoClient, err := argocd.NewRepoServerManager(base.restConfig, cfg.argocdNamespace, cfg.repoServerAddress, cfg.repoServerTimeoutSeconds,
		cfg.repoServerPlaintext, cfg.repoServerStrictTLS)
	if err != nil {
		return nil, err
	}
	return &RepoServerQueryController{
		BaseController: base,
		cfg:            cfg,
		repoClient:     repoClient,
	}, nil
}

// execute runs the graph query, computes the resources managed via Argo CD and update the resource.inclusions
// settings in the argocd-cm config map if it detects any new changes compared to the previous computed value or if its
// value is different from what is present in the argocd-cm config map.
// if the check interval time has not passed since the previous run, then the method returns without executing any queries.
func (r *RepoServerQueryController) execute() error {
	if !r.lastRunTime.IsZero() && time.Since(r.lastRunTime) < r.cfg.checkInterval {
		log.Info("skipping query executor due to last run not lapsed the check interval")
		return nil
	}
	r.lastRunTime = time.Now()
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

	if !*r.cfg.updateEnabled {
		if !r.previousGroupedKinds.Equal(&groupedKinds) {
			log.Info("direct update or argocd-cm is disabled, printing the output on terminal")
			resourceInclusionString := groupedKinds.String()
			if strings.HasPrefix(resourceInclusionString, "error:") {
				return fmt.Errorf("error in yaml string of resource.inclusions: %s", resourceInclusionString)
			}
			fmt.Printf("resource.inclusions: |\n%sresource.exclusions: ''\n", resourceInclusionString)
		} else {
			log.Infof("no changes detected in previously computed resource inclusions and current computed resource inclusions")
		}
	} else {
		if r.cfg.updateResourceKind == ArgoCDResourceKind {
			err = handleUpdateInArgoCDCR(r.argoCDClient, r.cfg.updateResourceName, r.cfg.argocdNamespace, groupedKinds)
			if err != nil {
				return err
			}
		} else {
			err = handleUpdateInCM(r.argoCDClient, r.cfg.argocdNamespace, groupedKinds)
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
