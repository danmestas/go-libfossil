package libfossil

import (
	"reflect"
	"testing"
)

func TestFslIDIsInt64(t *testing.T) {
	var id FslID = -1
	if id != -1 {
		t.Fatal("FslID should support negative values")
	}
	var big FslID = 1 << 33
	if big <= 0 {
		t.Fatal("FslID should support values > int32 max")
	}
}

func TestFslSizePhantom(t *testing.T) {
	if PhantomSize != -1 {
		t.Fatalf("PhantomSize = %d, want -1", PhantomSize)
	}
	var s FslSize = PhantomSize
	if s >= 0 {
		t.Fatal("FslSize should be able to represent -1 for phantoms")
	}
}

func TestFossilAppID(t *testing.T) {
	if FossilApplicationID != 252006673 {
		t.Fatalf("FossilApplicationID = %d, want 252006673", FossilApplicationID)
	}
}

func TestCreateOptsDefaults(t *testing.T) {
	opts := CreateOpts{}
	if opts.User != "" {
		t.Error("default User should be empty")
	}
}

func TestSyncOptsDefaults(t *testing.T) {
	opts := SyncOpts{}
	if opts.Push || opts.Pull {
		t.Error("default Push/Pull should be false")
	}
}

func TestLogOptsLimit(t *testing.T) {
	opts := LogOpts{Limit: 10}
	if opts.Limit != 10 {
		t.Errorf("got %d, want 10", opts.Limit)
	}
}

// TestLogEntryHasNoCursorField locks in the type split this test guards:
// LogEntry is Ancestry's result type, and Ancestry's cursor is always the
// zero value — structurally indistinguishable from a legitimate Timeline
// first call. Rather than document that as a hazard, LogEntry must not
// carry a Cursor field at all, so there is nothing to misuse in the first
// place. Timeline's pagination cursor lives on TimelineEntry instead (see
// TestTimelineEntryHasCursorField); feeding an Ancestry cursor to Timeline
// is a compile error because LogEntry.Cursor does not exist to obtain.
func TestLogEntryHasNoCursorField(t *testing.T) {
	typ := reflect.TypeOf(LogEntry{})
	if _, ok := typ.FieldByName("Cursor"); ok {
		t.Fatal("LogEntry must not have a Cursor field: Ancestry's result type should make cursor misuse unrepresentable, not merely documented as invalid")
	}
}

// TestTimelineEntryHasCursorField is the counterpart to
// TestLogEntryHasNoCursorField: Timeline is the operation with pagination
// semantics, so its result type — not the shared LogEntry — carries the
// always-valid Cursor.
func TestTimelineEntryHasCursorField(t *testing.T) {
	typ := reflect.TypeOf(TimelineEntry{})
	field, ok := typ.FieldByName("Cursor")
	if !ok {
		t.Fatal("TimelineEntry must have a Cursor field")
	}
	if field.Type != reflect.TypeOf(Cursor{}) {
		t.Fatalf("TimelineEntry.Cursor type = %v, want %v", field.Type, reflect.TypeOf(Cursor{}))
	}
}
