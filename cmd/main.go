package main

import (
	"os"
	"time"

	"github.com/anandf/resource-tracker/pkg/argocd"
	"github.com/anandf/resource-tracker/pkg/kube"
	"github.com/spf13/cobra"
)

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
