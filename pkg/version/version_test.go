package version

import (
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	ver := Version()
	if !strings.HasPrefix(ver, "v") {
		t.Errorf("Version should start with 'v', got: %s", ver)
	}
	if len(ver) < 3 {
		t.Errorf("Version format seems incorrect: %s", ver)
	}
}

func TestBinaryName(t *testing.T) {
	if BinaryName() != "argocd-resource-tracker" {
		t.Errorf("Unexpected binary name: %s", BinaryName())
	}
}

func TestUseragent(t *testing.T) {
	agent := Useragent()
	if !strings.Contains(agent, BinaryName()) || !strings.Contains(agent, Version()) {
		t.Errorf("Useragent format incorrect: %s", agent)
	}
}

func TestGitCommit(t *testing.T) {
	if GitCommit() != "unknown" {
		t.Errorf("Unexpected git commit: %s", GitCommit())
	}
}

func TestBuildDate(t *testing.T) {
	if BuildDate() != "1970-01-01T00:00:00Z" {
		t.Errorf("Unexpected build date: %s", BuildDate())
	}
}

func TestGoVersion(t *testing.T) {
	if GoVersion() == "" {
		t.Errorf("GoVersion should not be empty")
	}
}

func TestGoPlatform(t *testing.T) {
	if !strings.Contains(GoPlatform(), "/") {
		t.Errorf("GoPlatform format incorrect: %s", GoPlatform())
	}
}

func TestGoCompiler(t *testing.T) {
	if GoCompiler() == "" {
		t.Errorf("GoCompiler should not be empty")
	}
}
