package libfossil

import (
	"context"
	"fmt"
	"net/http"

	internalsync "github.com/danmestas/go-libfossil/internal/sync"
	"github.com/danmestas/go-libfossil/internal/xfer"
)

// SyncOpts configures a sync operation.
type SyncOpts struct {
	Push        bool
	Pull        bool
	UV          bool
	ProjectCode string
	ServerCode  string
	User        string
	Password    string
	PeerID      string // identifies this leaf agent instance
	MaxSend     int
	XTableSync  bool // enable extension table sync (peer_registry, etc.)
	Private     bool // enable private artifact sync
	Observer    SyncObserver
	Buggify     BuggifyChecker
}

// SyncResult describes the outcome of a sync session.
type SyncResult struct {
	Rounds       int
	FilesSent    int
	FilesRecvd   int
	UVFilesSent  int
	UVFilesRecvd int
	UVGimmesSent int
	BytesSent    int64
	BytesRecvd   int64
	Errors       []string
}

// HandleOpts configures server-side sync handling.
type HandleOpts struct {
	Observer SyncObserver
	Buggify  BuggifyChecker
}

// CloneOpts configures a clone operation.
type CloneOpts struct {
	User     string
	Password string
	Observer SyncObserver
	Buggify  BuggifyChecker // fault injection for DST (nil = no faults)
}

// transportAdapter bridges the public byte-level Transport to the internal
// xfer.Message-based sync.Transport.
type transportAdapter struct {
	pub Transport
}

func (a *transportAdapter) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	payload, err := req.Encode()
	if err != nil {
		return nil, fmt.Errorf("libfossil: transport encode: %w", err)
	}
	respBytes, err := a.pub.RoundTrip(ctx, payload)
	if err != nil {
		return nil, err
	}
	// This transport carries no Content-Type. Both ends are libfossil and reply
	// via Message.Encode, which always produces the §4.1 compressed container,
	// so the framing is fixed rather than negotiated per message.
	return xfer.Decode(respBytes, xfer.ContentTypeCompressed)
}

// observerAdapter bridges the public context-free SyncObserver to the internal
// context-carrying sync.Observer.
type observerAdapter struct {
	pub SyncObserver
}

func (a *observerAdapter) Started(ctx context.Context, info internalsync.SessionStart) context.Context {
	a.pub.Started(SessionStart{
		ProjectCode: info.ProjectCode,
		Push:        info.Push,
		Pull:        info.Pull,
		UV:          info.UV,
	})
	return ctx
}

func (a *observerAdapter) RoundStarted(ctx context.Context, round int) context.Context {
	a.pub.RoundStarted(round)
	return ctx
}

func (a *observerAdapter) RoundCompleted(_ context.Context, round int, stats internalsync.RoundStats) {
	a.pub.RoundCompleted(round, RoundStats{
		FilesSent:  stats.FilesSent,
		FilesRecvd: stats.FilesReceived,
		BytesSent:  int(stats.BytesSent),
		BytesRecvd: int(stats.BytesReceived),
		Gimmes:     stats.GimmesSent,
		IGots:      stats.IgotsSent,
	})
}

func (a *observerAdapter) Completed(_ context.Context, info internalsync.SessionEnd, _ error) {
	a.pub.Completed(SessionEnd{
		Rounds:     info.Rounds,
		FilesSent:  info.FilesSent,
		FilesRecvd: info.FilesRecvd,
	})
}

func (a *observerAdapter) Error(_ context.Context, err error) {
	a.pub.Error(err)
}

func (a *observerAdapter) HandleStarted(ctx context.Context, info internalsync.HandleStart) context.Context {
	a.pub.HandleStarted(HandleStart{
		RemoteAddr: info.RemoteAddr,
	})
	return ctx
}

func (a *observerAdapter) HandleCompleted(_ context.Context, info internalsync.HandleEnd) {
	a.pub.HandleCompleted(HandleEnd{
		FilesSent:  info.FilesSent,
		FilesRecvd: info.FilesReceived,
	})
}

func (a *observerAdapter) TableSyncStarted(_ context.Context, info internalsync.TableSyncStart) {
	a.pub.TableSyncStarted(TableSyncStart{
		Table: info.Table,
	})
}

func (a *observerAdapter) TableSyncCompleted(_ context.Context, info internalsync.TableSyncEnd) {
	a.pub.TableSyncCompleted(TableSyncEnd{
		Table:     info.Table,
		RowsSent:  info.Sent,
		RowsRecvd: info.Received,
	})
}

// adaptObserver returns an internal sync.Observer wrapping the public SyncObserver.
// Returns nil if pub is nil (internal packages treat nil as nop).
func adaptObserver(pub SyncObserver) internalsync.Observer {
	if pub == nil {
		return nil
	}
	return &observerAdapter{pub: pub}
}

// Sync runs a sync session against the given transport.
func (r *Repo) Sync(ctx context.Context, t Transport, opts SyncOpts) (*SyncResult, error) {
	adapter := &transportAdapter{pub: t}
	iOpts := internalsync.SyncOpts{
		Push:        opts.Push,
		Pull:        opts.Pull,
		UV:          opts.UV,
		ProjectCode: opts.ProjectCode,
		ServerCode:  opts.ServerCode,
		User:        opts.User,
		Password:    opts.Password,
		PeerID:      opts.PeerID,
		MaxSend:     opts.MaxSend,
		XTableSync:  opts.XTableSync,
		Private:     opts.Private,
		Observer:    adaptObserver(opts.Observer),
		Buggify:     opts.Buggify,
	}
	res, err := internalsync.Sync(ctx, r.inner, adapter, iOpts)
	if err != nil {
		return convertSyncResult(res), fmt.Errorf("libfossil: sync: %w", err)
	}
	return convertSyncResult(res), nil
}

func convertSyncResult(r *internalsync.SyncResult) *SyncResult {
	if r == nil {
		return nil
	}
	return &SyncResult{
		Rounds:       r.Rounds,
		FilesSent:    r.FilesSent,
		FilesRecvd:   r.FilesRecvd,
		UVFilesSent:  r.UVFilesSent,
		UVFilesRecvd: r.UVFilesRecvd,
		UVGimmesSent: r.UVGimmesSent,
		BytesSent:    r.BytesSent,
		BytesRecvd:   r.BytesRecvd,
		Errors:       r.Errors,
	}
}

// HandleSync processes an incoming xfer request (server-side).
// The payload is a raw xfer-encoded byte slice; the response is also raw bytes.
func (r *Repo) HandleSync(ctx context.Context, payload []byte) ([]byte, error) {
	return r.HandleSyncWithOpts(ctx, payload, HandleOpts{})
}

// HandleSyncWithOpts processes an incoming xfer request with optional configuration.
func (r *Repo) HandleSyncWithOpts(ctx context.Context, payload []byte, opts HandleOpts) ([]byte, error) {
	// The payload arrives without a Content-Type; a libfossil peer always sends
	// the §4.1 compressed container (Message.Encode), so the framing is fixed.
	msg, err := xfer.Decode(payload, xfer.ContentTypeCompressed)
	if err != nil {
		return nil, fmt.Errorf("libfossil: decode xfer: %w", err)
	}
	iOpts := internalsync.HandleOpts{
		Observer: adaptObserver(opts.Observer),
	}
	if opts.Buggify != nil {
		iOpts.Buggify = opts.Buggify
	}
	resp, err := internalsync.HandleSyncWithOpts(ctx, r.inner, msg, iOpts)
	if err != nil {
		return nil, fmt.Errorf("libfossil: handle sync: %w", err)
	}
	return resp.Encode()
}

// XferHandler returns an http.HandlerFunc that decodes Fossil xfer requests,
// dispatches to HandleSync, and encodes the response. Use this to compose
// a custom mux alongside operational endpoints (e.g., /healthz).
func (r *Repo) XferHandler() http.HandlerFunc {
	h := internalsync.HandleSync
	return internalsync.XferHandler(r.inner, h)
}

// ServeHTTP starts an HTTP server that accepts Fossil xfer requests.
// Blocks until ctx is cancelled.
func (r *Repo) ServeHTTP(ctx context.Context, addr string) error {
	return internalsync.ServeHTTP(ctx, addr, r.inner, internalsync.HandleSync)
}
