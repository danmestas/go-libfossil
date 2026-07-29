package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	moderncDriverImportPath = "github.com/danmestas/go-libfossil/db/driver/modernc"
	ncrucesDriverImportPath = "github.com/danmestas/go-libfossil/db/driver/ncruces"
)

func assertDriverDependencySelection(t *testing.T, tag, expected, unexpected string) {
	t.Helper()

	args := []string{"list", "-deps", "-f={{.ImportPath}}"}
	if tag != "" {
		args = append(args, "-tags", tag)
	}
	args = append(args, "./cmd/libfossil")

	cmd := exec.Command("go", args...)
	cmd.Dir = filepath.Join("..", "..")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list dependencies: %v\n%s", err, output)
	}

	dependencies := make(map[string]struct{})
	for _, dependency := range strings.Fields(string(output)) {
		dependencies[dependency] = struct{}{}
	}
	if _, ok := dependencies[expected]; !ok {
		t.Errorf("go list dependencies missing selected driver %q", expected)
	}
	if _, ok := dependencies[unexpected]; ok {
		t.Errorf("go list dependencies included unselected driver %q", unexpected)
	}
}
