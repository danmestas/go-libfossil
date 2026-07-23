package sync_test

// Manual measurement harness for issue #113 (serve-side wire amplification).
// Not a CI test: it needs a real `fossil` binary and builds a multi-hundred-
// artifact corpus, so it is gated behind WIRE_MEASURE=1 and skipped otherwise.
//
//   WIRE_MEASURE=1 GOWORK=off go test ./internal/sync/ \
//       -run TestMeasureServeWireAmplification -v -count=1 -timeout=300s
//
// It serves a corpus with this implementation's server, clones it with a stock
// fossil client through a byte-counting TCP proxy, and compares total bytes the
// server sent against the repository's own stored size. A fossil-serving-fossil
// control clones the same content from a native fossil server through the same
// proxy, so the two ratios are directly comparable.

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	libfossil "github.com/danmestas/go-libfossil/internal/fsltype"
	"github.com/danmestas/go-libfossil/internal/manifest"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/simio"
)

// countingProxy is a loopback TCP proxy that forwards one upstream and reports
// bytes carried in each direction. downstream is the server->client direction:
// the wire cost of serving.
type countingProxy struct {
	ln       net.Listener
	upstream string
	toClient atomic.Int64 // server -> client
	toServer atomic.Int64 // client -> server
}

func newCountingProxy(t *testing.T, upstream string) *countingProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &countingProxy{ln: ln, upstream: upstream}
	go p.serve()
	return p
}

func (p *countingProxy) addr() string { return p.ln.Addr().String() }
func (p *countingProxy) close()       { p.ln.Close() }

func (p *countingProxy) serve() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handle(client)
	}
}

func (p *countingProxy) handle(client net.Conn) {
	defer client.Close()
	server, err := net.Dial("tcp", p.upstream)
	if err != nil {
		return
	}
	defer server.Close()
	done := make(chan struct{}, 2)
	// server -> client (the direction we care about)
	go func() {
		n, _ := io.Copy(client, server)
		p.toClient.Add(n)
		done <- struct{}{}
	}()
	// client -> server
	go func() {
		n, _ := io.Copy(server, client)
		p.toServer.Add(n)
		done <- struct{}{}
	}()
	<-done
}

// buildCorpus checks the module's own .go files into srcRepo across several
// linear commits, giving a few hundred realistic (compressible) source blobs.
func buildCorpus(t *testing.T, srcRepo *repo.Repo, moduleRoot string) int {
	t.Helper()
	var files []manifest.File
	err := filepath.Walk(moduleRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(moduleRoot, path)
		if rerr != nil {
			return rerr
		}
		files = append(files, manifest.File{Name: rel, Content: data})
		return nil
	})
	if err != nil {
		t.Fatalf("walk corpus: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no .go files found for corpus")
	}

	const perCommit = 30
	var parent libfossil.FslID
	commits := 0
	for i := 0; i < len(files); i += perCommit {
		end := i + perCommit
		if end > len(files) {
			end = len(files)
		}
		mid, _, cerr := manifest.Checkin(srcRepo, manifest.CheckinOpts{
			Comment: fmt.Sprintf("corpus commit %d", commits),
			User:    "testuser",
			Parent:  parent,
			Files:   files[i:end],
		})
		if cerr != nil {
			t.Fatalf("checkin %d: %v", commits, cerr)
		}
		parent = mid
		commits++
	}
	if _, err := manifest.Crosslink(srcRepo); err != nil {
		t.Fatalf("crosslink: %v", err)
	}
	return len(files)
}

// waitPort dials addr until it accepts or the deadline passes.
func waitPort(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %s never accepted", addr)
}

func TestMeasureServeWireAmplification(t *testing.T) {
	if os.Getenv("WIRE_MEASURE") != "1" {
		t.Skip("set WIRE_MEASURE=1 to run the serve-side wire measurement")
	}
	fossilBin, err := exec.LookPath("fossil")
	if err != nil {
		t.Skip("no fossil binary on PATH")
	}
	moduleRoot := os.Getenv("MODULE_ROOT")
	if moduleRoot == "" {
		t.Fatal("set MODULE_ROOT to the go-libfossil checkout root")
	}

	dir := t.TempDir()
	srcRepo, err := repo.Create(filepath.Join(dir, "source.fossil"), "testuser", simio.CryptoRand{}, "")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	defer srcRepo.Close()

	nFiles := buildCorpus(t, srcRepo, moduleRoot)
	blobBytes := sumBlobContentBytes(t, srcRepo)
	var nBlobs int64
	if err := srcRepo.DB().QueryRow("SELECT count(*) FROM blob WHERE size >= 0").Scan(&nBlobs); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	var nDeltas, rawSize int64
	_ = srcRepo.DB().QueryRow("SELECT count(*) FROM delta").Scan(&nDeltas)
	_ = srcRepo.DB().QueryRow("SELECT coalesce(sum(size),0) FROM blob WHERE size >= 0").Scan(&rawSize)
	t.Logf("corpus: %d source files, %d artifacts, %d deltas, stored blob bytes=%d, raw(expanded) bytes=%d",
		nFiles, nBlobs, nDeltas, blobBytes, rawSize)

	// --- go-libfossil server, real fossil client, through the proxy ---
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	glfURL := serveRepo(ctx, t, srcRepo) // http://host:port
	glfUpstream := glfURL[len("http://"):]

	// Diagnostic: drive this implementation's own client so we can decode the
	// server's replies -- which card types carry the wire cost -- and sum the
	// exact compressed response bodies the server wrote (the true serve-side
	// wire cost, free of the real client's protocol-round shape).
	capture := &wireCaptureTransport{url: glfURL}
	diagRepo, _, derr := sync.Clone(ctx, filepath.Join(dir, "diag.fossil"), capture, sync.CloneOpts{})
	if derr != nil {
		t.Fatalf("diagnostic clone: %v", derr)
	}
	diagRepo.Close()
	t.Logf("server card mix (own-client clone): file=%d, cfile=%d, other=%d",
		capture.fileCards, capture.cfileCards, capture.otherCards)
	t.Logf("own-client clone response-body bytes: %d  (%.2fx stored)",
		capture.respBodyBytes, float64(capture.respBodyBytes)/float64(blobBytes))

	glfProxy := newCountingProxy(t, glfUpstream)
	defer glfProxy.close()

	glfClonePath := filepath.Join(dir, "glf-clone.fossil")
	cmd := exec.Command(fossilBin, "clone", "http://"+glfProxy.addr()+"/", glfClonePath)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fossil clone of go-libfossil server failed: %v\n%s", err, out)
	}
	glfSent := glfProxy.toClient.Load()

	// --- fossil-serving-fossil control on identical content ---
	// glf-clone.fossil is a native fossil repo with the same content; serve it
	// with a real fossil server and clone it back through a fresh proxy.
	port := freePort(t)
	controlServe := exec.CommandContext(ctx, fossilBin, "server", glfClonePath,
		"--localhost", "--port", fmt.Sprintf("%d", port))
	controlServe.Stdout = os.Stderr
	controlServe.Stderr = os.Stderr
	if err := controlServe.Start(); err != nil {
		t.Fatalf("start fossil server: %v", err)
	}
	defer func() { _ = controlServe.Process.Kill() }()
	controlUpstream := fmt.Sprintf("127.0.0.1:%d", port)
	waitPort(t, controlUpstream)

	controlProxy := newCountingProxy(t, controlUpstream)
	defer controlProxy.close()

	controlClonePath := filepath.Join(dir, "control-clone.fossil")
	cmd = exec.Command(fossilBin, "clone", "http://"+controlProxy.addr()+"/", controlClonePath)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fossil clone of fossil control server failed: %v\n%s", err, out)
	}
	controlSent := controlProxy.toClient.Load()

	// --- report ---
	t.Logf("=== issue #113 serve-side wire measurement ===")
	t.Logf("stored blob bytes (server's own on-disk content): %d", blobBytes)
	t.Logf("go-libfossil server -> client bytes: %d  (%.2fx stored)",
		glfSent, float64(glfSent)/float64(blobBytes))
	t.Logf("fossil control  server -> client bytes: %d  (%.2fx stored)",
		controlSent, float64(controlSent)/float64(blobBytes))
	t.Logf("go-libfossil vs fossil control: %.2fx",
		float64(glfSent)/float64(controlSent))
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
