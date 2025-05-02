package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/anandf/resource-tracker/pkg/argocd"
	"github.com/argoproj/argo-cd/v2/common"
	"github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/emirpasic/gods/sets/hashset"
	log "github.com/sirupsen/logrus"

	"github.com/anandf/resource-tracker/pkg/env"
	"github.com/anandf/resource-tracker/pkg/version"
	"github.com/spf13/cobra"
)

// newRunCommand implements "run" command
func newRunCommand() *cobra.Command {
	var cfg *ResourceTrackerConfig = &ResourceTrackerConfig{}
	var once bool
	var kubeConfig string

	var runCmd = &cobra.Command{
		Use:   "run",
		Short: "Runs the resource-tracker with a set of options",
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
			ctx := context.Background()
			var err error
			level, err := log.ParseLevel(cfg.LogLevel)
			if err != nil {
				return fmt.Errorf("failed to parse log level: %w", err)
			}
			log.SetLevel(level)
			resourceTrackerConfig, err := getKubeConfig(ctx, cfg.ArgocdNamespace, kubeConfig)
			if err != nil {
				log.Fatalf("could not create K8s client: %v", err)
			}
			cfg.ArgoClient, err = argocd.NewArgocd(resourceTrackerConfig, cfg.RepoServerAddress, cfg.RepoServerTimeoutSeconds, cfg.RepoServerPlaintext, cfg.RepoServerStrictTLS)
			if err != nil {
				return err
			}
			return runResourceTrackerLoop(cfg)
		},
	}
	runCmd.Flags().DurationVar(&cfg.CheckInterval, "interval", 2*time.Minute, "interval for how often to check for updates")
	runCmd.Flags().StringVar(&cfg.RepoServerAddress, "repo-server", env.GetStringVal("ARGOCD_REPO_SERVER", common.DefaultRepoServerAddr), "Repo server address.")
	runCmd.Flags().StringVar(&cfg.LogLevel, "loglevel", env.GetStringVal("RESOURCE_TRACKER_LOGLEVEL", "info"), "set the loglevel to one of trace|debug|info|warn|error")
	runCmd.Flags().IntVar(&cfg.RepoServerTimeoutSeconds, "repo-server-timeout-seconds", 60, "Repo server RPC call timeout seconds.")
	runCmd.Flags().BoolVar(&cfg.RepoServerPlaintext, "repo-server-plaintext", false, "Disable TLS on connections to repo server")
	runCmd.Flags().BoolVar(&cfg.RepoServerStrictTLS, "repo-server-strict-tls", false, "Whether to use strict validation of the TLS cert presented by the repo server")
	runCmd.Flags().StringVar(&kubeConfig, "kubeconfig", "", "full path to kube client configuration, i.e. ~/.kube/config")
	runCmd.Flags().StringVar(&cfg.ArgocdNamespace, "argocd-namespace", "", "namespace where ArgoCD runs in (current namespace by default)")

	return runCmd
}

func runResourceTrackerLoop(cfg *ResourceTrackerConfig) error {
	lastRun := time.Time{}
	for {
		if lastRun.IsZero() || time.Since(lastRun) > cfg.CheckInterval {
			runResourceTracker(cfg)
			lastRun = time.Now()
		}
		if cfg.CheckInterval == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}
func runResourceTracker(cfg *ResourceTrackerConfig) {
	log.Info("Starting resource tracking process...")
	apps, err := cfg.ArgoClient.ListApplications()
	if err != nil {
		log.Fatalf("Error while listing applications: %v", err)
	}
	apps = cfg.ArgoClient.FilterApplicationsByArgoCDNamespace(apps, cfg.ArgocdNamespace)
	if len(apps) == 0 {
		log.Warn("No applications found.")
		return
	}
	log.Infof("Fetched %d applications", len(apps))
	resourceChan := make(chan map[string]*hashset.Set)
	errChan := make(chan error)
	// Launch consumer (Tracker)
	go startResourceTrackerConsumer(cfg, resourceChan)
	var wg sync.WaitGroup
	for _, app := range apps {
		wg.Add(1)
		go func(app v1alpha1.Application) {
			defer wg.Done()
			appGroupedResources, err := cfg.ArgoClient.ProcessApplication(app, cfg.ArgocdNamespace)
			if err != nil {
				errChan <- err
				return
			}
			if appGroupedResources != nil {
				resourceChan <- appGroupedResources
			}
		}(app)
	}

	// Close channels after all apps processed
	go func() {
		wg.Wait()
		close(resourceChan)
	}()

	// Handle processing errors
	go func() {
		for err := range errChan {
			log.Errorf("Error processing application: %v", err)
		}
	}()
	log.Info("Resource tracking initiated for all applications.")
}

func startResourceTrackerConsumer(cfg *ResourceTrackerConfig, resourceChan <-chan map[string]*hashset.Set) {
	// Process resources from the channel
	// and update the tracked resources in the config
	// This is a blocking call, so it will keep running until the channel is closed
	for groupedResources := range resourceChan {
		cfg.ArgoClient.PopulateTrackedResources(groupedResources)
		err := cfg.ArgoClient.UpdateResourceInclusion(cfg.ArgocdNamespace)
		if err != nil {
			log.Errorf("Error updating resource inclusion: %v", err)
		}
	}
	log.Info("Resource channel has been closed. Resource Tracker process has completed successfully.")
}
