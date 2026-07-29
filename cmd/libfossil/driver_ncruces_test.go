//go:build test_ncruces

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNcrucesDriverDependencySelection(t *testing.T) {
	assertDriverDependencySelection(t, "test_ncruces", ncrucesDriverImportPath, moderncDriverImportPath)
}

func TestNcrucesRepoNewThenInfo(t *testing.T) {
	repositoryPath := filepath.Join(t.TempDir(), "smoke.fossil")

	newStdout, newStderr, newExitCode := runCLI(t, "repo", "new", repositoryPath)
	if newExitCode != 0 {
		t.Fatalf("repo new exited %d\nstdout:\n%s\nstderr:\n%s", newExitCode, newStdout, newStderr)
	}

	infoStdout, infoStderr, infoExitCode := runCLI(t, "repo", "info", "-R", repositoryPath)
	if infoExitCode != 0 {
		t.Fatalf("repo info exited %d\nstdout:\n%s\nstderr:\n%s", infoExitCode, infoStdout, infoStderr)
	}
	assertRepoInfoField := func(field string) {
		t.Helper()
		for _, line := range strings.Split(infoStdout, "\n") {
			if !strings.HasPrefix(line, field) {
				continue
			}
			if strings.TrimSpace(strings.TrimPrefix(line, field)) == "" {
				t.Fatalf("repo info printed an empty %s value\nstdout:\n%s\nstderr:\n%s", field, infoStdout, infoStderr)
			}
			return
		}
		t.Fatalf("repo info did not print a %s value\nstdout:\n%s\nstderr:\n%s", field, infoStdout, infoStderr)
	}
	assertRepoInfoField("project-code:")
	assertRepoInfoField("server-code:")
}
