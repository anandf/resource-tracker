package main

import (
	"os"
	"testing"

	"github.com/anandf/resource-tracker/pkg/env"
	"github.com/stretchr/testify/assert"
)

// TestNewRunCommand tests various flags and their default values.
func TestNewRunCommand(t *testing.T) {
	asser := assert.New(t)
	runCmd := newRepoServerCommand()
	asser.Contains(runCmd.Use, "run")
	asser.Greater(len(runCmd.Short), 25)
	asser.NotNil(runCmd.RunE)
	asser.Equal("2m0s", runCmd.Flag("interval").Value.String())
	asser.Equal(env.GetStringVal("RESOURCE_TRACKER_LOGLEVEL", "info"), runCmd.Flag("loglevel").Value.String())
	asser.Equal("", runCmd.Flag("kubeconfig").Value.String())
	asser.Equal("", runCmd.Flag("argocd-namespace").Value.String())
	asser.Nil(runCmd.Help())
}

// TestRootCmd tests main.go#newRootCommand.
func TestRootCmd(t *testing.T) {
	//remove the last element from os.Args so that it will not be taken as the arg to the image-updater command
	os.Args = os.Args[:len(os.Args)-1]
	err := newRootCommand()
	assert.Nil(t, err)
}
