package main

import (
	"os"

	"github.com/spf13/cobra"
)

// newRootCommand implements the root command of argocd-resource-tracker
func newRootCommand() error {
	var rootCmd = &cobra.Command{
		Use:   "argocd-resource-tracker",
		Short: "Argo CD Resource Tracker",
		Long:  "Argo CD Resource Tracker is a tool which analyzes the resource inclusions settings based on the resources managed by Argo Applications",
	}
	rootCmd.AddCommand(NewAnalyzeCommand())
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
