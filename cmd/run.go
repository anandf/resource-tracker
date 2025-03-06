package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/anandf/resource-tracker/pkg/argocd"
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

			// Enforce sane --max-concurrency values
			if cfg.MaxConcurrency < 1 {
				return fmt.Errorf("--max-concurrency must be greater than 1")
			}

			log.Infof("%s %s starting [loglevel:%s, interval:%s]",
				version.BinaryName(),
				version.Version(),
				strings.ToUpper(cfg.LogLevel),
				cfg.CheckInterval,
			)

			ctx := context.Background()
			var err error
			cfg.KubeClient, err = getKubeConfig(ctx, cfg.ArgocdNamespace, kubeConfig)
			if err != nil {
				log.Fatalf("could not create K8s client: %v", err)
			}
			if cfg.ClientOpts.ServerAddr == "" {
				cfg.ClientOpts.ServerAddr = fmt.Sprintf("argocd-server.%s", cfg.KubeClient.KubeClient.Namespace)
			}

			if token := os.Getenv("ARGOCD_TOKEN"); token != "" && cfg.ClientOpts.AuthToken == "" {
				cfg.ClientOpts.AuthToken = token
			}
			log.Infof("ArgoCD configuration: [apiKind=%s, server=%s, auth_token=%v, insecure=%v, grpc_web=%v, plaintext=%v]",
				cfg.ApplicationsAPIKind,
				cfg.ClientOpts.ServerAddr,
				cfg.ClientOpts.AuthToken != "",
				cfg.ClientOpts.Insecure,
				cfg.ClientOpts.GRPCWeb,
				cfg.ClientOpts.Plaintext,
			)
			return runResourceTrackerLoop(cfg)
		},
	}

	runCmd.Flags().StringVar(&cfg.ApplicationsAPIKind, "applications-api", env.GetStringVal("APPLICATIONS_API", applicationsAPIKindK8S), "API kind that is used to manage Argo CD applications ('kube' or 'argocd')")
	runCmd.Flags().BoolVar(&cfg.ClientOpts.GRPCWeb, "argocd-grpc-web", env.GetBoolVal("ARGOCD_GRPC_WEB", false), "use grpc-web for connection to ArgoCD")
	runCmd.Flags().BoolVar(&cfg.ClientOpts.Insecure, "argocd-insecure", env.GetBoolVal("ARGOCD_INSECURE", false), "(INSECURE) ignore invalid TLS certs for ArgoCD server")
	runCmd.Flags().BoolVar(&cfg.ClientOpts.Plaintext, "argocd-plaintext", env.GetBoolVal("ARGOCD_PLAINTEXT", false), "(INSECURE) connect without TLS to ArgoCD server")
	runCmd.Flags().StringVar(&cfg.ClientOpts.AuthToken, "argocd-auth-token", "", "use token for authenticating to ArgoCD (unsafe - consider setting ARGOCD_TOKEN env var instead)")
	runCmd.Flags().BoolVar(&cfg.DryRun, "dry-run", false, "run in dry-run mode. If set to true, do not perform any changes")
	runCmd.Flags().DurationVar(&cfg.CheckInterval, "interval", 2*time.Minute, "interval for how often to check for updates")
	runCmd.Flags().StringVar(&cfg.LogLevel, "loglevel", env.GetStringVal("IMAGE_UPDATER_LOGLEVEL", "info"), "set the loglevel to one of trace|debug|info|warn|error")
	runCmd.Flags().StringVar(&kubeConfig, "kubeconfig", "", "full path to kube client configuration, i.e. ~/.kube/config")
	runCmd.Flags().IntVar(&cfg.MaxConcurrency, "max-concurrency", 10, "maximum number of update threads to run concurrently")
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
	var argoClient argocd.ArgoCD
	var errorList []error
	// Initialize the correct ArgoCD client
	switch cfg.ApplicationsAPIKind {
	case applicationsAPIKindK8S:
		argoClient, err = argocd.NewK8SClient(cfg.KubeClient)
	case applicationsAPIKindArgoCD:
		argoClient, err = argocd.NewAPIClient(&cfg.ClientOpts)
	default:
		return result, fmt.Errorf("application API kind '%s' is not supported", cfg.ApplicationsAPIKind)
	}
	if err != nil {
		return result, fmt.Errorf("failed to create ArgoCD client: %w", err)
	}
	cfg.ArgoClient = argoClient
	// List applications
	apps, err := cfg.ArgoClient.ListApplications()
	if err != nil {
		return result, fmt.Errorf("error while listing applications: %w", err)
	}
	// Filter applications belonging to the 'argocd' namespace
	apps = argocd.FilterApplicationsByArgoCDNamespace(apps, "argocd")

	if len(apps) == 0 {
		log.Infof("No applications found in the 'argocd' namespace")
		return result, nil
	}
	// Process each application
	for _, app := range apps {
		log.Infof("Processing application: %s", app.Name)

		err := argocd.ProcessApplication(app, cfg.KubeClient)
		if err != nil {
			log.Errorf("Error processing application %s: %v", app.Name, err)
			result.NumErrors++
			errorList = append(errorList, fmt.Errorf("application %s: %w", app.Name, err))
			continue
		}

		result.NumApplicationsProcessed++
	}
	// Return aggregated errors if any
	if len(errorList) > 0 {
		return result, fmt.Errorf("encountered errors in processing: %v", errorList)
	}
	return result, nil
}
