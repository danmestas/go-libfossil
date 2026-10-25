package db

// DriverConfig defines a SQLite driver's name and DSN builder.
type DriverConfig struct {
	Name     string
	BuildDSN func(path string, pragmas map[string]string) string
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
