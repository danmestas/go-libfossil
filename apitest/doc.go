// Package apitest holds go-libfossil's black-box tests of the root package.
//
// The package holds no library code; everything is in its tests. Each test
// file is an external test package that imports github.com/danmestas/go-libfossil
// and uses only its exported API, so it can neither depend on nor reach into
// the root package's internals. Tests that need those internals stay beside
// the code they exercise, in the repository root.
package apitest
