package otel_test

import (
	"fmt"
	"testing"

	libfossil "github.com/danmestas/go-libfossil"
	otelobserver "github.com/danmestas/go-libfossil/observer/otel"
)

func TestSyncObserverImplementsInterface(t *testing.T) {
	var obs libfossil.SyncObserver = otelobserver.NewSyncObserver()
	if obs == nil {
		t.Fatal("NewSyncObserver returned nil")
	}
}

func TestCheckoutObserverImplementsInterface(t *testing.T) {
	var obs libfossil.CheckoutObserver = otelobserver.NewCheckoutObserver()
	if obs == nil {
		t.Fatal("NewCheckoutObserver returned nil")
	}
}

func TestSyncObserverDoesNotPanic(t *testing.T) {
	obs := otelobserver.NewSyncObserver()

	// Full lifecycle — should not panic even without an OTel exporter configured.
	obs.Started(libfossil.SessionStart{
		ProjectCode: "test-project",
		Push:        true,
		Pull:        true,
		UV:          false,
	})
	obs.RoundStarted(1)
	obs.RoundCompleted(1, libfossil.RoundStats{
		FilesSent: 3, FilesRecvd: 2,
		BytesSent: 1024, BytesRecvd: 512,
		Gimmes: 1, IGots: 2,
	})
	obs.HandleStarted(libfossil.HandleStart{RemoteAddr: "127.0.0.1"})
	obs.HandleCompleted(libfossil.HandleEnd{FilesSent: 1, FilesRecvd: 1})
	obs.TableSyncStarted(libfossil.TableSyncStart{Table: "config"})
	obs.TableSyncCompleted(libfossil.TableSyncEnd{Table: "config", RowsSent: 5, RowsRecvd: 3})
	obs.Error(nil)
	obs.Error(fmt.Errorf("test error"))
	obs.Completed(libfossil.SessionEnd{Rounds: 1, FilesSent: 3, FilesRecvd: 2})
}

func TestCheckoutObserverDoesNotPanic(t *testing.T) {
	obs := otelobserver.NewCheckoutObserver()

	// Extract lifecycle
	obs.ExtractStarted(libfossil.ExtractStart{RID: 42, Dir: "/tmp/checkout"})
	obs.ExtractFileCompleted("README.md", libfossil.ChangeAdded)
	obs.ExtractFileCompleted("main.go", libfossil.ChangeModified)
	obs.ExtractCompleted(libfossil.ExtractEnd{FilesWritten: 2})

	// Scan lifecycle
	obs.ScanStarted("/tmp/checkout")
	obs.ScanCompleted(libfossil.ScanEnd{FilesScanned: 10})

	// Commit lifecycle
	obs.CommitStarted(libfossil.CommitStart{Comment: "test", User: "alice", Files: 2})
	obs.CommitCompleted(libfossil.CommitEnd{UUID: "abc123def456", RID: 43})

	// Errors
	obs.Error(nil)
	obs.Error(fmt.Errorf("test error"))
}

func TestSyncObserverString(t *testing.T) {
	obs := otelobserver.NewSyncObserver()
	s := obs.String()
	if s == "" {
		t.Fatal("String() returned empty")
	}
}

func TestCheckoutObserverString(t *testing.T) {
	obs := otelobserver.NewCheckoutObserver()
	s := obs.String()
	if s == "" {
		t.Fatal("String() returned empty")
	}
}
