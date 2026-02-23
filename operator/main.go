package main

import (
	"os"

	"github.com/spf13/cobra"
)

// newRootCommand implements the root command of argocd-resource-tracker
func newRootCommand() error {
	rootCmd := &cobra.Command{
		Use:   "argocd-resource-tracker-operator",
		Short: "Dynamically update resource.inclusions based on the resources managed by Argo Applications",
	}

	rootCmd.AddCommand(newGraphQueryCommand())
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
