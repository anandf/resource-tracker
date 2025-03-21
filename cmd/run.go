package main

import (
	"context"
	"fmt"
	"math"
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
	runCmd.Flags().IntVar(&cfg.RepoServerTimeoutSeconds, "repo-server-timeout-seconds", env.ParseNumFromEnv("ARGOCD_APPLICATION_CONTROLLER_REPO_SERVER_TIMEOUT_SECONDS", 60, 0, math.MaxInt64), "Repo server RPC call timeout seconds.")
	runCmd.Flags().BoolVar(&cfg.RepoServerPlaintext, "repo-server-plaintext", env.GetBoolVal("ARGOCD_APPLICATION_CONTROLLER_REPO_SERVER_PLAINTEXT", false), "Disable TLS on connections to repo server")
	runCmd.Flags().BoolVar(&cfg.RepoServerStrictTLS, "repo-server-strict-tls", env.GetBoolVal("ARGOCD_APPLICATION_CONTROLLER_REPO_SERVER_STRICT_TLS", false), "Whether to use strict validation of the TLS cert presented by the repo server")
	runCmd.Flags().StringVar(&kubeConfig, "kubeconfig", "", "full path to kube client configuration, i.e. ~/.kube/config")
	runCmd.Flags().StringVar(&cfg.ArgocdNamespace, "argocd-namespace", "", "namespace where ArgoCD runs in (current namespace by default)")

	return runCmd
}

func runResourceTrackerLoop(cfg *ResourceTrackerConfig) error {
	lastRun := time.Time{}
	for {
		if lastRun.IsZero() || time.Since(lastRun) > cfg.CheckInterval {
			result, err := runResourceTracker(cfg)
			if err != nil {
				log.Errorf("Error updating applications: %v", err)
			} else {
				log.Infof("Resource Tracker Loop: Processed %d applications with %d errors", result.NumApplicationsProcessed, result.NumErrors)
			}
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
func runResourceTracker(cfg *ResourceTrackerConfig) (argocd.ResourceTrackerResult, error) {
	result := argocd.ResourceTrackerResult{}
	var err error
	var errorList []error
	apps, err := cfg.ArgoClient.ListApplications()
	if err != nil {
		return result, fmt.Errorf("error while listing applications: %w", err)
	}

	// Filter applications belonging to the 'argocd' namespace
	apps = cfg.ArgoClient.FilterApplicationsByArgoCDNamespace(apps, cfg.ArgocdNamespace)

	if len(apps) == 0 {
		log.Infof("No applications found in the 'argocd' namespace")
		return result, nil
	}
	// Process each application
	for _, app := range apps {
		err := cfg.ArgoClient.ProcessApplication(app)
		if err != nil {
			result.NumErrors++
			errorList = append(errorList, fmt.Errorf("application %s: %w", app.Name, err))
		}
		result.NumApplicationsProcessed++
	}
	if len(errorList) > 0 {
		return result, fmt.Errorf("encountered errors: %v", errorList)
	}
	return result, nil
}
