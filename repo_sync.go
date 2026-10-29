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
	// ShareLinks is fossil's --share-links mode (§9.5): a pulling session
	// asks the server for the URLs of other copies of the repository and
	// keeps each valid one as config link:URL. Off by default.
	ShareLinks bool
	Observer   SyncObserver
	Buggify    BuggifyChecker
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
	// Elided counts the artifacts left absent under the store cone, and
	// ConeServed the replies whose peer acknowledged applying it (pragma
	// cone-served, draft-fossil-sparse-cone-00 §6.3); the rest of a sparse
	// store's rounds were served densely and filtered locally.
	Elided, ConeServed int
	// HorizonServed counts the replies whose peer served the store's
	// horizon (pragma horizon-served, draft-fossil-horizon-00 §6.2.5); the
	// other rounds were classified locally. StructuralServed counts those
	// served at the structural level, and HorizonFallback reports a
	// structural store that fell back to the content horizon for good (§6.3).
	HorizonServed    int
	StructuralServed int
	HorizonFallback  bool
	// HorizonHistoryURL is the interface outside the sync protocol at which
	// the origin says history below a horizon can be queried (pragma
	// horizon-served URL, draft-fossil-horizon-00 §6.2.5), the last a reply
	// named; "" when none did. An origin names it with
	// SettingHorizonHistoryURL.
	HorizonHistoryURL string
	// HorizonRemedy is set when HorizonFallback is: it names the horizon,
	// says the manifests below it are now wanted, and names where they are
	// held, the dense origin at HorizonHistoryURL when a reply named one. A
	// peer that is itself at a horizon does not hold them
	// (draft-fossil-horizon-00 §6.2.3, §6.3, §6.4).
	HorizonRemedy string
}

// HandleOpts configures server-side sync handling.
type HandleOpts struct {
	Observer SyncObserver
	Buggify  BuggifyChecker
	// LoginCookie is the value of the request's x-f-l-c cookie, which
	// carries the login card once the client has seen a server-version
	// that reads it (§6.5). This server announces such a release, so fossil
	// and go-libfossil clients move their login there after the first
	// round: an embedder that serves HandleSyncWithOpts over HTTP must pass
	// the cookie through, or those rounds arrive as nobody. XferHandler and
	// ServeHTTP read it themselves.
	LoginCookie string
	// RemoteAddr is the address the request came from, recorded in the
	// rcvfrom row of whatever it pushes, as fossil records REMOTE_ADDR.
	// XferHandler and ServeHTTP fill it themselves.
	RemoteAddr string
	// MaxStoredBytes, when positive, is the most bytes one pushed artifact
	// may occupy as this repository stores it (compressed, and as a delta
	// when pushed as one). Set it when the host's storage caps a single
	// value: a push carrying a bigger artifact is refused whole with an
	// error naming the artifact, its stored size and the limit, and the
	// file and check-in when this repository already holds a check-in
	// listing it, so nothing from that request is stored. 0, the default,
	// stores any size. Clones and pulls served from the repository are not
	// limited. XferHandler and ServeHTTP serve without a budget: a host that
	// needs one serves HandleSyncWithOpts itself.
	MaxStoredBytes int
}

// CloneOpts configures a clone operation.
type CloneOpts struct {
	User     string
	Password string
	Observer SyncObserver
	Buggify  BuggifyChecker // fault injection for DST (nil = no faults)
	// PullPhantoms, when set, makes Clone fetch the artifacts the clone
	// rounds left missing with a follow-up pull session, and fail with an
	// error if any are still missing after it. That pull departs from
	// Section 8.2 of the sync draft, which sends no clone, pull or push card
	// after clone_seqno 0, so it is off by default: like fossil(1)'s clone,
	// Clone then succeeds with those artifacts left as phantoms.
	PullPhantoms bool
	// StoreCone, when non-nil, clones straight into a sparse store: only the
	// artifacts the cone covers are fetched, and the cone becomes the store's
	// policy. With a horizon (NewHorizonCone) it clones a content-horizon
	// store, eliding the blobs below the horizon; CloneResult.HorizonServed
	// counts the replies whose server cut at the horizon, the others being
	// classified locally. A server that will not serve a sparse clone
	// refuses it (SettingPartialMirror).
	StoreCone *Cone
	// SeedKey, when set, is the origin's public key, kept as the clone's
	// SettingHorizonSeedKey: every structural horizon seed is verified against
	// it, in this clone and every later pull and rebuild.
	SeedKey string
	// Horizon, when set, is the horizon to clone at, its Checkin a full hash
	// or a name: "tip", a branch or tag. HorizonDepth steps it back that
	// many check-ins along primary parents. A name, or any depth, is
	// resolved by the origin (draft-fossil-horizon-00 §6.2.7); against one
	// that does not, the clone resolves it over what it received at the
	// content horizon and says so in CloneResult. Only the resolved check-in
	// is stored. StoreCone then gives the path members alone: a horizon on
	// both is an error, as is a depth without a Horizon.
	Horizon      *Horizon
	HorizonDepth int
}

// transportAdapter bridges the public byte-level Transport to the internal
// xfer.Message-based sync.Transport.
type transportAdapter struct {
	pub Transport
}

// PeerAddr reports the address of the peer the wrapped transport last
// reached, when it knows one, for the receipt of what a sync receives.
func (a *transportAdapter) PeerAddr() string {
	if p, ok := a.pub.(interface{ PeerAddr() string }); ok {
		return p.PeerAddr()
	}
	return ""
}

func (a *transportAdapter) Exchange(ctx context.Context, req *xfer.Message) (*xfer.Message, error) {
	// A transport that can set the login cookie gets the login there once
	// the server reads it (§6.5); any other sends it in the body.
	if lt, ok := a.pub.(loginCookieTransport); ok {
		if sent, cookie := req.SplitLoginCookie(); cookie != "" {
			payload, err := sent.Encode()
			if err != nil {
				return nil, fmt.Errorf("libfossil: transport encode: %w", err)
			}
			respBytes, contentType, err := lt.roundTripLoginCookie(ctx, payload, cookie)
			if err != nil {
				return nil, err
			}
			return xfer.DecodeReply(respBytes, contentType)
		}
	}
	payload, err := req.Encode()
	if err != nil {
		return nil, fmt.Errorf("libfossil: transport encode: %w", err)
	}
	// When the transport can report the reply's framing signal (e.g. an HTTP
	// Content-Type), decode by that signal — a §4-conforming peer may reply
	// with either framing, and a real Fossil clone reply is uncompressed. The
	// §4-8 reply rule lives in xfer.DecodeReply: a missing type is the
	// compressed container, a foreign type (an HTML error page) is refused.
	if ft, ok := a.pub.(FramedTransport); ok {
		respBytes, contentType, err := ft.RoundTripFramed(ctx, payload)
		if err != nil {
			return nil, err
		}
		return xfer.DecodeReply(respBytes, contentType)
	}
	respBytes, err := a.pub.RoundTrip(ctx, payload)
	if err != nil {
		return nil, err
	}
	// The transport reports no framing signal. Both ends are libfossil and
	// reply via Message.Encode, which always produces the §4.1 compressed
	// container, so the framing is fixed by construction rather than negotiated.
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
		ShareLinks:  opts.ShareLinks,
		Observer:    adaptObserver(opts.Observer),
		Buggify:     opts.Buggify,
	}
	// A transport that names its peer (OriginNamer) keys the horizon cursor;
	// any other keeps none, and its requests announce in full, which §6.2.6
	// allows.
	iOpts.Origin = syncOrigin(t)
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
		Rounds:            r.Rounds,
		FilesSent:         r.FilesSent,
		FilesRecvd:        r.FilesRecvd,
		UVFilesSent:       r.UVFilesSent,
		UVFilesRecvd:      r.UVFilesRecvd,
		UVGimmesSent:      r.UVGimmesSent,
		BytesSent:         r.BytesSent,
		BytesRecvd:        r.BytesRecvd,
		Errors:            r.Errors,
		Elided:            r.Elided,
		ConeServed:        r.ConeServed,
		HorizonServed:     r.HorizonServed,
		StructuralServed:  r.StructuralServed,
		HorizonFallback:   r.HorizonFallback,
		HorizonHistoryURL: r.HorizonHistoryURL,
		HorizonRemedy:     r.HorizonRemedy,
	}
}

// HandleSync processes an incoming xfer request (server-side).
// The payload is a raw xfer-encoded byte slice; the response is also raw bytes.
func (r *Repo) HandleSync(ctx context.Context, payload []byte) ([]byte, error) {
	return r.HandleSyncWithOpts(ctx, payload, HandleOpts{})
}

// HandleSyncWithOpts processes an incoming xfer request with optional configuration.
func (r *Repo) HandleSyncWithOpts(ctx context.Context, payload []byte, opts HandleOpts) ([]byte, error) {
	if opts.MaxStoredBytes < 0 {
		return nil, fmt.Errorf("libfossil: HandleOpts.MaxStoredBytes is %d, must not be negative",
			opts.MaxStoredBytes)
	}
	// The payload arrives without a Content-Type; a libfossil peer always sends
	// the §4.1 compressed container (Message.Encode), so the framing is fixed.
	msg, err := xfer.DecodeRequest(payload, xfer.ContentTypeCompressed, opts.LoginCookie)
	if err != nil {
		return nil, fmt.Errorf("libfossil: decode xfer: %w", err)
	}
	iOpts := internalsync.HandleOpts{
		Observer:       adaptObserver(opts.Observer),
		RemoteAddr:     opts.RemoteAddr,
		MaxStoredBytes: opts.MaxStoredBytes,
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
// a custom mux alongside operational endpoints (e.g., /healthz). It serves
// with default HandleOpts, so no HandleOpts.MaxStoredBytes budget applies.
func (r *Repo) XferHandler() http.HandlerFunc {
	h := internalsync.HandleSync
	return internalsync.XferHandler(r.inner, h)
}

// ServeHTTP starts an HTTP server that accepts Fossil xfer requests.
// Blocks until ctx is cancelled.
//
// Requests are authorized against the user table: one without a login card
// gets nobody's capabilities, which in a new repository allow clone and pull
// but not push. Grant more with SetCaps. Like XferHandler it serves with
// default HandleOpts, so no HandleOpts.MaxStoredBytes budget applies.
func (r *Repo) ServeHTTP(ctx context.Context, addr string) error {
	return internalsync.ServeHTTP(ctx, addr, r.inner, internalsync.HandleSync)
}
