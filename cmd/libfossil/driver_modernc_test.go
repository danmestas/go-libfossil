//go:build !test_ncruces

package main

import "testing"

func TestDefaultDriverDependencySelection(t *testing.T) {
	assertDriverDependencySelection(t, "", moderncDriverImportPath, ncrucesDriverImportPath)
}
