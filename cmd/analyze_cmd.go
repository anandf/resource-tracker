package main

import (
	"fmt"
	"strings"

	"github.com/anandf/resource-tracker/pkg/common"
	"github.com/anandf/resource-tracker/pkg/env"
	"github.com/anandf/resource-tracker/pkg/version"
	"github.com/avitaltamir/cyphernetes/pkg/core"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// QueryConfig holds all the flag-based configuration for the analysis commands.
type QueryConfig struct {
	applicationName      string
	applicationNamespace string
	globalQuery          *bool
	logLevel             string
	kubeConfig           string
	argocdNamespace      string
	strategy             string // <--- Renamed from relationSource
	allApps              bool
}

// ResourceTrackerBackend defines the common interface for different analysis strategies.
type ResourceTrackerBackend interface {
	Execute(cfg *QueryConfig) (*common.GroupedResourceKinds, error)
}

// NewAnalyzeCommand creates the 'analyze' command, which is the primary entrypoint.
func NewAnalyzeCommand() *cobra.Command {
	cfg := &QueryConfig{}

	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Analyze resource relationships and dependencies for ArgoCD applications",
		Long:  "Analyze resource relationships and dependencies for ArgoCD applications. Can process a single app or all apps.",
		RunE: func(cmd *cobra.Command, args []string) error {
			log.Infof("%s %s starting [loglevel:%s]",
				version.BinaryName(),
				version.Version(),
				strings.ToUpper(cfg.logLevel),
			)
			level, err := log.ParseLevel(cfg.logLevel)
			if err != nil {
				return fmt.Errorf("failed to parse log level: %w", err)
			}
			log.SetLevel(level)
			core.LogLevel = cfg.logLevel
			// Support app specified as "namespace/name" similar to `argocd app get ns/name`.
			// If provided, override the separate applicationNamespace flag.
			if cfg.applicationName != "" && !cfg.allApps {
				if parts := strings.SplitN(cfg.applicationName, "/", 2); len(parts) == 2 {
					cfg.applicationNamespace = parts[0]
					cfg.applicationName = parts[1]
					log.WithFields(log.Fields{
						"applicationName":      cfg.applicationName,
						"applicationNamespace": cfg.applicationNamespace,
					}).Debug("Parsed application from namespace/name syntax")
				}
			}

			// If user requested all apps, ensure graph runs in non-global mode
			if cfg.allApps {
				*cfg.globalQuery = false
			}
			var backend ResourceTrackerBackend
			switch cfg.strategy {
			case "graph":
				backend, err = NewGraphBackend(cfg)
				if err != nil {
					return err
				}
			case "dynamic":
				backend, err = NewDynamicBackend(cfg)
				if err != nil {
					return err
				}
			default:
				return fmt.Errorf("invalid strategy: %s (must be 'graph' or 'dynamic')", cfg.strategy)
			}

			groupedKinds, err := backend.Execute(cfg)
			if err != nil {
				return err
			}
			printInclusions(groupedKinds)
			return nil
		},
	}
	cmd.Flags().StringVar(&cfg.logLevel, "loglevel", env.GetStringVal("RESOURCE_TRACKER_LOGLEVEL", "info"), "set the loglevel to one of trace|debug|info|warn|error")
	cmd.Flags().StringVarP(&cfg.applicationName, "app", "a", "", "Application name (required for single app analysis). Supports 'namespace/name' syntax.")
	cmd.Flags().StringVarP(&cfg.applicationNamespace, "app-namespace", "N", "argocd", "Application namespace")
	cmd.Flags().StringVarP(&cfg.argocdNamespace, "namespace", "n", "argocd", "ArgoCD namespace")
	cmd.Flags().StringVar(&cfg.kubeConfig, "kubeconfig", "", "Path to kubeconfig file for cluster access")
	cmd.Flags().BoolVar(&cfg.allApps, "all-apps", false, "Analyze all applications in the namespace")
	cmd.Flags().StringVar(&cfg.strategy, "strategy", "dynamic", "Analysis strategy: 'dynamic' (OwnerRef walking) or 'graph' (Cyphernetes)")
	//TODO: Can we remove this flag? and just use the all-apps flag?
	cfg.globalQuery = cmd.Flags().Bool("global", true, "perform graph query without listing applications and finding children for each application (graph only)")

	return cmd
}

func printInclusions(groupedKinds *common.GroupedResourceKinds) {
	resourceInclusionString := groupedKinds.String()
	if strings.HasPrefix(resourceInclusionString, "error:") {
		log.Errorf("error generating resource.inclusions: %s", resourceInclusionString)
		return
	}
	log.Infof("resource.inclusions: |\n%s", resourceInclusionString)
}
