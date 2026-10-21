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

// preparedSparseService is an internal lifecycle prototype, not a bounded-work
// server. Preparation inflates the whole store; warm membership classification
// still examines all parsed references, and pull inventories still scan the
// blob table. No public listener uses it and no capability is advertised.
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
		s.facts, err = content.LoadConeReferenceFacts(ctx, bound.DB())
		return err
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
	s.mu.Lock()
	defer s.mu.Unlock()
	var resp *xfer.Message
	applied := false
	err := s.repo.WithBoundTx(func(bound *repo.Repo) error {
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
			usable = err == nil && version == s.version
		}
		var facts *content.ConeReferenceFacts
		if usable {
			facts = s.facts
		}
		var err error
		resp, err = handleSyncWithFacts(ctx, bound, req, s.opts, facts, !usable)
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
