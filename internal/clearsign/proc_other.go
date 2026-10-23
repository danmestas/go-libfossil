//go:build !unix && !js

package clearsign

import "os/exec"

// ownGroup leaves cmd as it is: outside Unix, cancellation kills the
// command alone.
func ownGroup(*exec.Cmd) {}
