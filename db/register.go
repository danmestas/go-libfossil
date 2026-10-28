package db

// DriverConfig defines a SQLite driver's name and DSN builder.
type DriverConfig struct {
	Name     string
	BuildDSN func(path string, pragmas map[string]string) string

	// MaxValueBytes is the largest single value the backend accepts, for a
	// hosted SQLite (such as a Durable Object's) that caps value size. A
	// stored blob larger than this is written across rows of a blobpart
	// table, with its length left in blob.content as an INTEGER; hashes,
	// deltas and the wire protocol are unchanged. Zero, the default,
	// disables this: every value is stored inline and no blobpart table is
	// created, so a local store stays byte-identical to stock fossil's.
	//
	// The setting is process-wide, like the registration that carries it:
	// it applies to every repository the process writes. Only file blobs are
	// chunked; unversioned.content is still written as one value and stays
	// subject to the backend's cap.
	//
	// A store holding blobpart rows cannot be read by stock fossil, which
	// sees those blobs' content as a bare integer. Set it only for hosted
	// backends. A nonzero value must be at least 1,000,000, the part size.
	MaxValueBytes int
}

var registered *DriverConfig

// Register registers a SQLite driver for use by Open/OpenWith.
// Must be called exactly once (typically from a driver package's init()).
// Panics if called more than once.
func Register(cfg DriverConfig) {
	if registered != nil {
		panic("db: driver already registered")
	}
	if cfg.Name == "" {
		panic("db: driver name must not be empty")
	}
	if cfg.BuildDSN == nil {
		panic("db: BuildDSN must not be nil")
	}
	// Parts are a fixed 1,000,000 bytes (internal/blob.PartBytes), so a
	// smaller limit could not hold even one of them.
	if cfg.MaxValueBytes < 0 || (cfg.MaxValueBytes > 0 && cfg.MaxValueBytes < 1_000_000) {
		panic("db: MaxValueBytes must be 0 or at least 1,000,000")
	}
	registered = &cfg
	if registered.Name == "" || registered.BuildDSN == nil {
		panic("db: registration postcondition failed")
	}
}

// RegisteredDriver returns a copy of the currently registered driver config, or nil if none.
func RegisteredDriver() *DriverConfig {
	if registered == nil {
		return nil
	}
	cfg := *registered
	return &cfg
}
