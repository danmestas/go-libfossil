package sync

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/auth"
	"github.com/danmestas/go-libfossil/internal/cone"
	"github.com/danmestas/go-libfossil/internal/repo"
)

// TestCorpusClone clones a copy of the repository LIBFOSSIL_CORPUS names over
// HTTP from an in-process handler, once per mode LIBFOSSIL_CORPUS_CLONE lists
// (comma separated: dense, cone, content, structural; default dense), and
// logs each clone's wall and CPU time. It exists to be run with -cpuprofile.
// CPU is the process's, server and client together. LIBFOSSIL_CORPUS_DEADLINE
// (a duration) bounds each clone, so a slow one still writes its profile;
// LIBFOSSIL_CORPUS_OUT keeps the clones in that directory for comparison.
func TestCorpusClone(t *testing.T) {
	corpus := os.Getenv("LIBFOSSIL_CORPUS")
	if corpus == "" {
		t.Skip("set LIBFOSSIL_CORPUS to a .fossil repository to run")
	}
	modes := strings.Split(os.Getenv("LIBFOSSIL_CORPUS_CLONE"), ",")
	if modes[0] == "" {
		modes = []string{"dense"}
	}
	var deadline time.Duration
	if s := os.Getenv("LIBFOSSIL_CORPUS_DEADLINE"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			t.Fatalf("LIBFOSSIL_CORPUS_DEADLINE: %v", err)
		}
		deadline = d
	}
	out := os.Getenv("LIBFOSSIL_CORPUS_OUT")
	if out == "" {
		out = t.TempDir()
	}
	work := filepath.Join(t.TempDir(), "origin.fossil")
	if err := corpusCopy(work, corpus); err != nil {
		t.Fatalf("copy corpus: %v", err)
	}
	origin, err := repo.Open(work)
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	t.Cleanup(func() { origin.Close() })
	if err := auth.UpdateCaps(origin.DB(), "nobody", "gijoryz"); err != nil {
		t.Fatal(err)
	}
	var tip string
	if err := origin.DB().QueryRow(`SELECT b.uuid FROM event e JOIN blob b ON b.rid=e.objid
		WHERE e.type='ci' ORDER BY e.mtime DESC LIMIT 1`).Scan(&tip); err != nil {
		t.Fatal(err)
	}
	horizon := corpusAncestor(t, origin, tip, 1000)
	server := httptest.NewServer(XferHandler(origin, HandleSync))
	t.Cleanup(server.Close)

	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			storeCone := corpusCloneCone(t, mode, horizon)
			ctx := context.Background()
			if deadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, deadline)
				defer cancel()
			}
			path := filepath.Join(out, mode+".fossil")
			cpu0, wall0 := processCPU(t), time.Now()
			r, res, err := Clone(ctx, path, &HTTPTransport{URL: server.URL},
				CloneOpts{StoreCone: storeCone})
			cpu, wall := processCPU(t)-cpu0, time.Since(wall0)
			if err != nil {
				t.Fatalf("%s clone after %s wall, %s CPU: %v", mode, wall, cpu, err)
			}
			defer r.Close()
			t.Logf("%s clone: %d rounds, %d blobs, %s wall, %s CPU",
				mode, res.Rounds, res.BlobsRecvd, wall.Round(time.Millisecond),
				cpu.Round(time.Millisecond))
		})
	}
}

// corpusCloneCone is the store cone a TestCorpusClone mode clones with.
func corpusCloneCone(t *testing.T, mode, horizon string) *cone.Cone {
	t.Helper()
	var c *cone.Cone
	var err error
	switch mode {
	case "dense":
		return nil
	case "cone":
		c, err = cone.New([]string{"src"}, nil)
	case "content", "structural":
		depth := cone.DepthContent
		if mode == "structural" {
			depth = cone.DepthStructural
		}
		var h cone.Horizon
		if h, err = cone.NewHorizon(horizon, cone.RuleDescendants, depth); err == nil {
			c, err = cone.NewWithHorizon(nil, nil, h)
		}
	default:
		t.Fatalf("unknown clone mode %q", mode)
	}
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// processCPU is the user and system time this process has used.
func processCPU(t *testing.T) time.Duration {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatal(err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
