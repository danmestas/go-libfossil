package sync

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"

	"github.com/danmestas/go-libfossil/db"
	"github.com/danmestas/go-libfossil/internal/content"
	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// preparedSparseService is an internal indexed-clone prototype. Bounded,
// off-path preparation reads the store and builds a cone-independent successor
// index. Warm clone selection does not scan excluded gaps. Selected content
// reconstruction still uses the dense pipeline's existing allocation behavior;
// indexing alone does not bound that work. Ordinary pull still scans the blob
// inventory. This is not a bounded-work general sync server. No public listener
// uses it and no capability is advertised.
//
// Liveness limit: the service never re-prepares, so once the store's
// generation changes every later request is served dense; a single foreign
// commit disables sparse serving until the service is rebuilt.
//
// The caller owns the root repository and must keep it open. Its path must
// still name the opened database, and must not be replaced during preparation:
// repo does not expose the opened file identity needed to verify that premise.
// This service owns only its independent observer pool/connection and immutable
// raw-reference facts.
// It never creates or advertises ephemeral content-addressed artifacts.
type preparedSparseService struct {
	mu       sync.Mutex
	repo     *repo.Repo
	opts     HandleOpts
	observer *sparseObserver
	pinned   *sql.Conn
	identity os.FileInfo
	version  int64
	facts    *content.ConeReferenceFacts
	index    *content.ConeSuccessors
	lastWork content.SuccessorWork
	prepWork content.ReferenceWork
	closed   bool
}

func prepareSparseService(
	ctx context.Context, r *repo.Repo, opts HandleOpts,
) (*preparedSparseService, error) {
	if ctx == nil {
		panic("sync.prepareSparseService: nil context")
	}
	if r == nil {
		panic("sync.prepareSparseService: nil repository")
	}
	// The ordinary handler lazily creates this table even for a read-only
	// exchange. Do that explicit preparation before pinning the generation.
	if err := repo.EnsureSyncSchema(r.DB()); err != nil {
		return nil, err
	}
	observer, err := newSparseObserver(ctx, r.Path())
	if err != nil {
		return nil, err
	}
	// A protocol fatal can roll back with a nil Go error, after a gimme has
	// expanded an uncommitted RID. Never share RID-keyed cache state across
	// this service's transactions, including dense mutation fallbacks.
	opts.ContentCache = nil
	s := &preparedSparseService{repo: r, opts: opts, observer: observer, pinned: observer.conn}
	err = r.WithBoundTx(func(bound *repo.Repo) error {
		var err error
		s.identity, err = os.Stat(r.Path())
		if err != nil {
			return err
		}
		s.version, err = observer.version(ctx)
		if err != nil {
			return err
		}
		return s.prepareIndex(ctx, bound)
	})
	if err != nil {
		return nil, errors.Join(err, observer.Close())
	}
	return s, nil
}

// handle returns whether prepared facts were selected, solely for internal
// verification. Stale/unsupported requests degrade without a cold re-scan;
// their original cone still reaches the sparse-origin coverage/refusal gate.
func (s *preparedSparseService) handle(
	ctx context.Context, req *xfer.Message,
) (*xfer.Message, bool, error) {
	if ctx == nil {
		panic("sync.preparedSparseService.handle: nil context")
	}
	if req == nil {
		panic("sync.preparedSparseService.handle: nil request")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	var resp *xfer.Message
	s.lastWork = content.SuccessorWork{}
	applied := false
	err := s.repo.WithBoundTx(func(bound *repo.Repo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		c, coneErr := requestCone(req.Cards)
		usable := !s.closed && coneErr == nil && c != nil
		if usable {
			usable = s.observer.conn == s.pinned && preparedSparseRead(req)
		}
		if usable {
			identity, err := os.Stat(s.repo.Path())
			usable = err == nil && os.SameFile(s.identity, identity)
		}
		if usable {
			version, err := s.observer.version(ctx)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			usable = err == nil && version == s.version
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		var proof *sparsePreparedProof
		if usable {
			proof = &sparsePreparedProof{facts: s.facts, index: s.index}
		}
		var err error
		resp, err = handleSyncWithFacts(ctx, bound, req, s.opts, proof, !usable)
		if proof != nil {
			s.lastWork = proof.work
		}
		applied = usable && err == nil
		return err
	})
	return resp, applied, err
}

func preparedSparseRead(req *xfer.Message) bool {
	for _, card := range req.Cards {
		switch c := card.(type) {
		case *xfer.CloneCard, *xfer.PullCard, *xfer.GimmeCard, *xfer.LoginCard, *xfer.CookieCard:
		case *xfer.PragmaCard:
			switch c.Name {
			case "client-version", "send-submanifests", "cone", "cone-not":
			default:
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (s *preparedSparseService) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.facts = nil
	s.index = nil
	return s.observer.Close()
}

// A separate read-only pool cannot consume the repository's sole pool slot.
// data_version is read only through this one pinned, never-writing connection.
type sparseObserver struct {
	pool *sql.DB
	conn *sql.Conn
}

func newSparseObserver(ctx context.Context, path string) (*sparseObserver, error) {
	pool, err := db.OpenSQL(path, db.OpenConfig{}, map[string]string{"mode": "ro"})
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(1)
	conn, err := pool.Conn(ctx)
	if err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	return &sparseObserver{pool: pool, conn: conn}, nil
}

func (o *sparseObserver) version(ctx context.Context) (int64, error) {
	var version int64
	err := o.conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version)
	return version, err
}

func (o *sparseObserver) Close() error {
	err := o.conn.Close()
	if errors.Is(err, sql.ErrConnDone) {
		err = nil
	}
	return errors.Join(err, o.pool.Close())
}
