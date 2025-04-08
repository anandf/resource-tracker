package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anandf/resource-tracker/pkg/argocd"
	"github.com/argoproj/argo-cd/v2/common"
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

// Main loop for resource-tracker
func runResourceTracker(cfg *ResourceTrackerConfig) {
	log.Info("Starting resource tracking process...")
	// Fetch all applications
	log.Debug("Fetching all applications from ArgoCD...")
	apps, err := cfg.ArgoClient.ListApplications()
	if err != nil {
		log.Fatalf("Error while listing applications: %v", err)
	}
	// Filter applications in the specified Argo CD namespace
	apps = cfg.ArgoClient.FilterApplicationsByArgoCDNamespace(apps, cfg.ArgocdNamespace)
	totalApps := len(apps)
	if len(apps) == 0 {
		log.Warn("No applications found in namespace.")
		return
	}
	log.Infof("Fetched %d applications from ArgoCD.", totalApps)
	// Process all applications
	errorList := cfg.ArgoClient.ProcessApplication(apps, cfg.ArgocdNamespace)
	// Calculate successfully processed applications
	successfulApps := totalApps - len(errorList)
	log.Infof("Processed %d out of %d applications successfully.", successfulApps, totalApps)
	// Log all individual errors separately
	if len(errorList) > 0 {
		log.Warnf("Encountered %d errors while processing applications.", len(errorList))
		for _, appErr := range errorList {
			log.Errorf("Application processing error: %v", appErr)
		}
	} else {
		log.Info("All applications processed successfully without errors.")
	}
	log.Info("Resource tracking process completed.")
}
