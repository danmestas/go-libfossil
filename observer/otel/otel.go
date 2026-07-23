// Package otel provides OpenTelemetry implementations of the libfossil
// SyncObserver and CheckoutObserver interfaces. Import this package only
// when you want OTel instrumentation — it pulls in the OTel SDK dependency.
package otel

import (
	"context"
	"fmt"
	"sync"

	libfossil "github.com/danmestas/go-libfossil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const scopeName = "github.com/danmestas/go-libfossil/observer/otel"

// --- SyncObserver ---

// SyncObserver implements libfossil.SyncObserver using OpenTelemetry spans and metrics.
type SyncObserver struct {
	tracer trace.Tracer
	meter  metric.Meter

	sessionCounter metric.Int64Counter
	roundCounter   metric.Int64Counter
	fileSentCounter metric.Int64Counter
	fileRecvCounter metric.Int64Counter
	errorCounter   metric.Int64Counter

	mu          sync.Mutex
	sessionSpan trace.Span
	sessionCtx  context.Context
	roundSpan   trace.Span
	roundCtx    context.Context
	handleSpan  trace.Span
}

// NewSyncObserver returns a SyncObserver that records spans and metrics
// using the OTel global tracer and meter providers.
func NewSyncObserver() *SyncObserver {
	tracer := otel.Tracer(scopeName)
	meter := otel.Meter(scopeName)

	sessionCounter, _ := meter.Int64Counter("sync.sessions",
		metric.WithDescription("Total sync sessions"))
	roundCounter, _ := meter.Int64Counter("sync.rounds",
		metric.WithDescription("Total sync rounds"))
	fileSentCounter, _ := meter.Int64Counter("sync.files_sent",
		metric.WithDescription("Total files sent"))
	fileRecvCounter, _ := meter.Int64Counter("sync.files_received",
		metric.WithDescription("Total files received"))
	errorCounter, _ := meter.Int64Counter("sync.errors",
		metric.WithDescription("Total sync errors"))

	return &SyncObserver{
		tracer:          tracer,
		meter:           meter,
		sessionCounter:  sessionCounter,
		roundCounter:    roundCounter,
		fileSentCounter: fileSentCounter,
		fileRecvCounter: fileRecvCounter,
		errorCounter:    errorCounter,
	}
}

func (o *SyncObserver) Started(info libfossil.SessionStart) {
	o.mu.Lock()
	defer o.mu.Unlock()

	ctx, span := o.tracer.Start(context.Background(), "sync.session",
		trace.WithAttributes(
			attribute.String("sync.project_code", info.ProjectCode),
			attribute.Bool("sync.push", info.Push),
			attribute.Bool("sync.pull", info.Pull),
			attribute.Bool("sync.uv", info.UV),
		))
	o.sessionCtx = ctx
	o.sessionSpan = span
	o.sessionCounter.Add(ctx, 1)
}

func (o *SyncObserver) RoundStarted(round int) {
	o.mu.Lock()
	defer o.mu.Unlock()

	parent := o.sessionCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, span := o.tracer.Start(parent, "sync.round",
		trace.WithAttributes(attribute.Int("sync.round", round)))
	o.roundCtx = ctx
	o.roundSpan = span
	o.roundCounter.Add(ctx, 1)
}

func (o *SyncObserver) RoundCompleted(round int, stats libfossil.RoundStats) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.roundSpan != nil {
		o.roundSpan.SetAttributes(
			attribute.Int("sync.files_sent", stats.FilesSent),
			attribute.Int("sync.files_received", stats.FilesRecvd),
			attribute.Int("sync.bytes_sent", stats.BytesSent),
			attribute.Int("sync.bytes_received", stats.BytesRecvd),
			attribute.Int("sync.gimmes", stats.Gimmes),
			attribute.Int("sync.igots", stats.IGots),
		)
		o.roundSpan.End()
		o.roundSpan = nil
		o.roundCtx = nil
	}

	ctx := o.sessionCtx
	if ctx == nil {
		ctx = context.Background()
	}
	o.fileSentCounter.Add(ctx, int64(stats.FilesSent))
	o.fileRecvCounter.Add(ctx, int64(stats.FilesRecvd))
}

func (o *SyncObserver) Completed(info libfossil.SessionEnd) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.sessionSpan != nil {
		o.sessionSpan.SetAttributes(
			attribute.Int("sync.total_rounds", info.Rounds),
			attribute.Int("sync.total_files_sent", info.FilesSent),
			attribute.Int("sync.total_files_received", info.FilesRecvd),
		)
		o.sessionSpan.End()
		o.sessionSpan = nil
		o.sessionCtx = nil
	}
}

func (o *SyncObserver) Error(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err == nil {
		return
	}
	o.errorCounter.Add(context.Background(), 1)
	if o.sessionSpan != nil {
		o.sessionSpan.RecordError(err)
	}
}

func (o *SyncObserver) HandleStarted(info libfossil.HandleStart) {
	o.mu.Lock()
	defer o.mu.Unlock()

	_, span := o.tracer.Start(context.Background(), "sync.handle",
		trace.WithAttributes(
			attribute.String("sync.remote_addr", info.RemoteAddr),
		))
	o.handleSpan = span
}

func (o *SyncObserver) HandleCompleted(info libfossil.HandleEnd) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.handleSpan != nil {
		o.handleSpan.SetAttributes(
			attribute.Int("sync.files_sent", info.FilesSent),
			attribute.Int("sync.files_received", info.FilesRecvd),
		)
		o.handleSpan.End()
		o.handleSpan = nil
	}
}

func (o *SyncObserver) TableSyncStarted(info libfossil.TableSyncStart) {
	// Table sync spans are lightweight — log as events on the session span.
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.sessionSpan != nil {
		o.sessionSpan.AddEvent("table_sync.started",
			trace.WithAttributes(attribute.String("sync.table", info.Table)))
	}
}

func (o *SyncObserver) TableSyncCompleted(info libfossil.TableSyncEnd) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.sessionSpan != nil {
		o.sessionSpan.AddEvent("table_sync.completed",
			trace.WithAttributes(
				attribute.String("sync.table", info.Table),
				attribute.Int("sync.rows_sent", info.RowsSent),
				attribute.Int("sync.rows_received", info.RowsRecvd),
			))
	}
}

// --- CheckoutObserver ---

// CheckoutObserver implements libfossil.CheckoutObserver using OpenTelemetry spans and metrics.
type CheckoutObserver struct {
	tracer trace.Tracer
	meter  metric.Meter

	extractCounter metric.Int64Counter
	commitCounter  metric.Int64Counter
	scanCounter    metric.Int64Counter
	errorCounter   metric.Int64Counter

	mu          sync.Mutex
	extractSpan trace.Span
	extractCtx  context.Context
	scanSpan    trace.Span
	commitSpan  trace.Span
	commitCtx   context.Context
}

// NewCheckoutObserver returns a CheckoutObserver that records spans and metrics
// using the OTel global tracer and meter providers.
func NewCheckoutObserver() *CheckoutObserver {
	tracer := otel.Tracer(scopeName)
	meter := otel.Meter(scopeName)

	extractCounter, _ := meter.Int64Counter("checkout.extracts",
		metric.WithDescription("Total checkout extractions"))
	commitCounter, _ := meter.Int64Counter("checkout.commits",
		metric.WithDescription("Total commits"))
	scanCounter, _ := meter.Int64Counter("checkout.scans",
		metric.WithDescription("Total working-tree scans"))
	errorCounter, _ := meter.Int64Counter("checkout.errors",
		metric.WithDescription("Total checkout errors"))

	return &CheckoutObserver{
		tracer:         tracer,
		meter:          meter,
		extractCounter: extractCounter,
		commitCounter:  commitCounter,
		scanCounter:    scanCounter,
		errorCounter:   errorCounter,
	}
}

func (o *CheckoutObserver) ExtractStarted(info libfossil.ExtractStart) {
	o.mu.Lock()
	defer o.mu.Unlock()

	ctx, span := o.tracer.Start(context.Background(), "checkout.extract",
		trace.WithAttributes(
			attribute.Int64("checkout.rid", info.RID),
			attribute.String("checkout.dir", info.Dir),
		))
	o.extractCtx = ctx
	o.extractSpan = span
	o.extractCounter.Add(ctx, 1)
}

func (o *CheckoutObserver) ExtractFileCompleted(name string, change libfossil.UpdateChange) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.extractSpan != nil {
		o.extractSpan.AddEvent("file",
			trace.WithAttributes(
				attribute.String("checkout.file", name),
				attribute.String("checkout.change", string(change)),
			))
	}
}

func (o *CheckoutObserver) ExtractCompleted(info libfossil.ExtractEnd) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.extractSpan != nil {
		o.extractSpan.SetAttributes(
			attribute.Int("checkout.files_written", info.FilesWritten),
		)
		o.extractSpan.End()
		o.extractSpan = nil
		o.extractCtx = nil
	}
}

func (o *CheckoutObserver) ScanStarted(dir string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	_, span := o.tracer.Start(context.Background(), "checkout.scan",
		trace.WithAttributes(attribute.String("checkout.dir", dir)))
	o.scanSpan = span
	o.scanCounter.Add(context.Background(), 1)
}

func (o *CheckoutObserver) ScanCompleted(info libfossil.ScanEnd) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.scanSpan != nil {
		o.scanSpan.SetAttributes(
			attribute.Int("checkout.files_scanned", info.FilesScanned),
		)
		o.scanSpan.End()
		o.scanSpan = nil
	}
}

func (o *CheckoutObserver) CommitStarted(info libfossil.CommitStart) {
	o.mu.Lock()
	defer o.mu.Unlock()

	ctx, span := o.tracer.Start(context.Background(), "checkout.commit",
		trace.WithAttributes(
			attribute.String("checkout.user", info.User),
			attribute.Int("checkout.files", info.Files),
		))
	o.commitCtx = ctx
	o.commitSpan = span
	o.commitCounter.Add(ctx, 1)
}

func (o *CheckoutObserver) CommitCompleted(info libfossil.CommitEnd) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.commitSpan != nil {
		uuid := info.UUID
		if len(uuid) > 10 {
			uuid = uuid[:10]
		}
		o.commitSpan.SetAttributes(
			attribute.String("checkout.uuid", uuid),
			attribute.Int64("checkout.rid", info.RID),
		)
		o.commitSpan.End()
		o.commitSpan = nil
		o.commitCtx = nil
	}
}

func (o *CheckoutObserver) Error(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err == nil {
		return
	}
	o.errorCounter.Add(context.Background(), 1)
	if o.extractSpan != nil {
		o.extractSpan.RecordError(err)
	}
	if o.commitSpan != nil {
		o.commitSpan.RecordError(err)
	}
}

// compile-time interface checks
var _ libfossil.SyncObserver = (*SyncObserver)(nil)
var _ libfossil.CheckoutObserver = (*CheckoutObserver)(nil)

// String returns a human-readable description.
func (o *SyncObserver) String() string     { return fmt.Sprintf("OTelSyncObserver(%s)", scopeName) }
func (o *CheckoutObserver) String() string { return fmt.Sprintf("OTelCheckoutObserver(%s)", scopeName) }
