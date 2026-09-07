// Package config reads every knob from the environment, applies the documented
// defaults, and fails fast with every problem listed at once.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	ListenAddr         string
	StorageBackend     string
	StorageDSN         string
	StorageMaxMB       int
	RetentionEvents    time.Duration
	RetentionChanges   time.Duration
	RetentionSnapshots time.Duration
	IncidentPodThresh  int
	IncidentNSThresh   int
	IncidentWindow     time.Duration
	BaselineStable     time.Duration
	LogLevel           string
}

// Defaults are the documented values in docs/CONFIGURATION.md. A bare
// helm install must run on these alone.
func Defaults() Config {
	return Config{
		ListenAddr:         ":8080",
		StorageBackend:     "sqlite",
		StorageDSN:         "/data/recorder.db",
		StorageMaxMB:       4096,
		RetentionEvents:    720 * time.Hour,
		RetentionChanges:   720 * time.Hour,
		RetentionSnapshots: 168 * time.Hour,
		IncidentPodThresh:  3,
		IncidentNSThresh:   2,
		IncidentWindow:     10 * time.Minute,
		BaselineStable:     15 * time.Minute,
		LogLevel:           "info",
	}
}

// FromEnv overlays environment variables on Defaults and validates.
func FromEnv() (Config, error) { return parse(os.Getenv) }

func parse(get func(string) string) (Config, error) {
	c := Defaults()
	var errs []error
	str := func(key string, dst *string) {
		if v := get(key); v != "" {
			*dst = v
		}
	}
	num := func(key string, dst *int) {
		if v := get(key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %q is not an integer", key, v))
				return
			}
			*dst = n
		}
	}
	dur := func(key string, dst *time.Duration) {
		if v := get(key); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %q is not a duration (use e.g. 720h, 30m)", key, v))
				return
			}
			*dst = d
		}
	}
	minutes := func(key string, dst *time.Duration) {
		if v := get(key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %q is not an integer number of minutes", key, v))
				return
			}
			*dst = time.Duration(n) * time.Minute
		}
	}

	str("LISTEN_ADDR", &c.ListenAddr)
	str("STORAGE_BACKEND", &c.StorageBackend)
	str("STORAGE_DSN", &c.StorageDSN)
	num("STORAGE_MAX_MB", &c.StorageMaxMB)
	dur("RETENTION_EVENTS", &c.RetentionEvents)
	dur("RETENTION_CHANGES", &c.RetentionChanges)
	dur("RETENTION_SNAPSHOTS", &c.RetentionSnapshots)
	num("INCIDENT_POD_THRESHOLD", &c.IncidentPodThresh)
	num("INCIDENT_NS_THRESHOLD", &c.IncidentNSThresh)
	minutes("INCIDENT_WINDOW_MIN", &c.IncidentWindow)
	minutes("BASELINE_STABLE_MIN", &c.BaselineStable)
	str("LOG_LEVEL", &c.LogLevel)

	errs = append(errs, c.validate()...)
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return c, nil
}

func (c Config) validate() []error {
	var errs []error
	switch c.StorageBackend {
	case "sqlite":
	case "postgres":
		errs = append(errs, errors.New("STORAGE_BACKEND=postgres is not implemented in this version; use sqlite"))
	default:
		errs = append(errs, fmt.Errorf("STORAGE_BACKEND must be sqlite or postgres, got %q", c.StorageBackend))
	}
	if c.StorageDSN == "" {
		errs = append(errs, errors.New("STORAGE_DSN must not be empty"))
	}
	if c.StorageMaxMB < 16 {
		errs = append(errs, fmt.Errorf("STORAGE_MAX_MB must be >= 16, got %d", c.StorageMaxMB))
	}
	for k, d := range map[string]time.Duration{
		"RETENTION_EVENTS": c.RetentionEvents, "RETENTION_CHANGES": c.RetentionChanges,
		"RETENTION_SNAPSHOTS": c.RetentionSnapshots, "INCIDENT_WINDOW_MIN": c.IncidentWindow,
		"BASELINE_STABLE_MIN": c.BaselineStable,
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive, got %s", k, d))
		}
	}
	if c.IncidentPodThresh < 1 || c.IncidentNSThresh < 1 {
		errs = append(errs, errors.New("INCIDENT_POD_THRESHOLD and INCIDENT_NS_THRESHOLD must be >= 1"))
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("LOG_LEVEL must be debug|info|warn|error, got %q", c.LogLevel))
	}
	return errs
}
