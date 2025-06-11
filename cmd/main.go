package main

import (
	"os"
	"time"

	"github.com/anandf/resource-tracker/pkg/argocd"
	"github.com/spf13/cobra"
)

// ResourceTrackerConfig contains global configuration and required runtime data
type ResourceTrackerConfig struct {
	ArgocdNamespace          string
	CheckInterval            time.Duration
	ArgoClient               argocd.ArgoCD
	LogLevel                 string
	RepoServerAddress        string
	RepoServerPlaintext      bool
	RepoServerStrictTLS      bool
	RepoServerTimeoutSeconds int
}

// newRootCommand implements the root command of argocd-resource-tracker
func newRootCommand() error {
	var rootCmd = &cobra.Command{
		Use:   "argocd-resource-tracker",
		Short: "Dynamically update resource.inclusions based on the resources managed by Argo Applications",
	}
	rootCmd.AddCommand(newRunCommand())
	rootCmd.AddCommand(newRunQueryCommand())
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
