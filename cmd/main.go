package main

import (
	"os"
	"time"

	"github.com/anandf/resource-tracker/pkg/argocd"
	"github.com/anandf/resource-tracker/pkg/kube"
	"github.com/spf13/cobra"
)

var lastRun time.Time

// Default ArgoCD server address when running in same cluster as ArgoCD
const defaultArgoCDServerAddr = "argocd-server.argocd"

// Default path to registry configuration
const defaultRegistriesConfPath = "/app/config/registries.conf"

// Default path to Git commit message template
const defaultCommitTemplatePath = "/app/config/commit.template"

const applicationsAPIKindK8S = "kube"
const applicationsAPIKindArgoCD = "argocd"

// ResourceTrackerConfig contains global configuration and required runtime data
type ResourceTrackerConfig struct {
	ApplicationsAPIKind string
	ClientOpts          argocd.ClientOptions
	ArgocdNamespace     string
	DryRun              bool
	CheckInterval       time.Duration
	ArgoClient          argocd.ArgoCD
	LogLevel            string
	MaxConcurrency      int
	HealthPort          int
	MetricsPort         int
	RegistriesConf      string
	AppNamePatterns     []string
	AppLabel            string
	KubeClient          *kube.ResourceTrackerKubeClient
}

// newRootCommand implements the root command of argocd-image-updater
func newRootCommand() error {
	var rootCmd = &cobra.Command{
		Use:   "argocd-resource-tracker",
		Short: "Dynamically update resource.inclusions based on the resources managed by Argo Applications",
	}
	rootCmd.AddCommand(newRunCommand())
	rootCmd.AddCommand(newVersionCommand())
	err := rootCmd.Execute()
	return err
}

func main() {
	err := newRootCommand()
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
