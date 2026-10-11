package conformance

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// fossil(1) loads a script the way go-libfossil does: each artifact stored
// with test-content-put and crosslinked with test-crosslink, in script
// order. Both commands need an open checkout and a user; neither takes -R.

// fossilTimeout bounds one fossil(1) command.
const fossilTimeout = 60 * time.Second

// fossilRepo is a fossil(1) repository with a checkout open on it.
type fossilRepo struct {
	path     string
	checkout string
}

// run runs fossil(1) in dir as user u and returns its combined output.
func (f *fossilReference) run(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fossilTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "USER=u")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("fossil %s: %v: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

// emptyRepo makes, once per reference, a repository holding no artifact:
// `fossil new` writes an initial check-in, so its blobs are deleted and the
// derived tables rebuilt empty.
func (f *fossilReference) emptyRepo() (string, error) {
	path := filepath.Join(f.dir, "empty.fossil")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	steps := [][]string{
		{"new", "-A", "u", path},
		{"sql", "-R", path, "DELETE FROM blob; DELETE FROM delta; DELETE FROM rcvfrom;"},
		{"rebuild", path},
	}
	for _, step := range steps {
		if _, err := f.run(f.dir, step...); err != nil {
			os.Remove(path)
			return "", err
		}
	}
	return path, nil
}

// newRepo copies the empty repository and opens a checkout on the copy.
func (f *fossilReference) newRepo() (fossilRepo, error) {
	empty, err := f.emptyRepo()
	if err != nil {
		return fossilRepo{}, err
	}
	f.count++
	dir := filepath.Join(f.dir, fmt.Sprintf("repo-%d", f.count))
	repo := fossilRepo{path: filepath.Join(dir, "f.fossil"), checkout: filepath.Join(dir, "ck")}
	if err := os.MkdirAll(repo.checkout, 0o755); err != nil {
		return fossilRepo{}, err
	}
	if err := copyFile(empty, repo.path); err != nil {
		return fossilRepo{}, err
	}
	if _, err := f.run(repo.checkout, "open", repo.path); err != nil {
		return fossilRepo{}, err
	}
	return repo, nil
}

// loadScript loads a script into a new repository one artifact at a time,
// crosslinking each as it arrives. fossil's crosslink refuses an artifact
// it cannot parse by returning without effect, so test-crosslink succeeds
// for those too.
func (f *fossilReference) loadScript(script rsScript, doctor *rsDoctor) (fossilRepo, error) {
	repo, err := f.newRepo()
	if err != nil {
		return fossilRepo{}, err
	}
	for i, a := range script.arts {
		if doctor != nil && doctor.at == i {
			if _, err := f.run(repo.checkout, "sql", script.resolve(doctor.sql)); err != nil {
				return fossilRepo{}, err
			}
		}
		file := filepath.Join(filepath.Dir(repo.path), fmt.Sprintf("artifact-%02d", i))
		if err := os.WriteFile(file, a.data, 0o644); err != nil {
			return fossilRepo{}, err
		}
		if _, err := f.run(repo.checkout, "test-content-put", file); err != nil {
			return fossilRepo{}, err
		}
		if _, err := f.run(repo.checkout, "test-crosslink", a.uuid); err != nil {
			return fossilRepo{}, err
		}
	}
	return repo, nil
}

// rebuildCopy runs `fossil rebuild` on a copy of repo. wrote reports
// whether the rebuild stored artifacts of its own: it tags a root that has
// no branch tag, so its result is then not derived from the script alone.
func (f *fossilReference) rebuildCopy(repo fossilRepo) (path string, wrote bool, err error) {
	path = strings.TrimSuffix(repo.path, ".fossil") + "-rebuilt.fossil"
	if err := copyFile(repo.path, path); err != nil {
		return "", false, err
	}
	before, err := f.blobCount(path)
	if err != nil {
		return "", false, err
	}
	if _, err := f.run(filepath.Dir(path), "rebuild", path); err != nil {
		return "", false, err
	}
	after, err := f.blobCount(path)
	if err != nil {
		return "", false, err
	}
	return path, after != before, nil
}

func (f *fossilReference) blobCount(path string) (string, error) {
	output, err := f.run(filepath.Dir(path), "sql", "-R", path, "SELECT count(*) FROM blob")
	return strings.TrimSpace(output), err
}
