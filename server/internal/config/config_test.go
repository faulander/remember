package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Load(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:8080" || cfg.DatabasePath != "data/sqlite/remember.db" ||
		cfg.BlobRoot != "data/blobs" || cfg.StagingPath != "data/staging" {
		t.Errorf("defaults = %#v", cfg)
	}
	if cfg.ShutdownTimeout != 15*time.Second || cfg.DatabaseBusy != 5*time.Second {
		t.Errorf("timeout defaults = %#v", cfg)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		EnvListenAddr:        "0.0.0.0:9090",
		EnvDatabasePath:      "/data/sqlite/remember.db",
		EnvBlobRoot:          "/data/blobs",
		EnvStagingPath:       "/data/staging",
		EnvReadHeaderTimeout: "2s",
		EnvReadTimeout:       "10s",
		EnvWriteTimeout:      "20s",
		EnvIdleTimeout:       "45s",
		EnvShutdownTimeout:   "9s",
		EnvDatabaseBusy:      "750ms",
	}
	cfg, err := Load(func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != values[EnvListenAddr] || cfg.DatabasePath != values[EnvDatabasePath] ||
		cfg.BlobRoot != values[EnvBlobRoot] || cfg.StagingPath != values[EnvStagingPath] {
		t.Errorf("overrides = %#v", cfg)
	}
	if cfg.DatabaseBusy != 750*time.Millisecond || cfg.ShutdownTimeout != 9*time.Second {
		t.Errorf("duration overrides = %#v", cfg)
	}
}

func TestLoadRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	tests := map[string]map[string]string{
		"empty address":        {EnvListenAddr: ""},
		"missing port":         {EnvListenAddr: "localhost"},
		"bad port":             {EnvListenAddr: "localhost:99999"},
		"empty database":       {EnvDatabasePath: " "},
		"database DSN":         {EnvDatabasePath: "file:test.db?mode=rwc"},
		"empty blob root":      {EnvBlobRoot: " "},
		"same storage roots":   {EnvBlobRoot: "data/same", EnvStagingPath: "data/same"},
		"blob equals database": {EnvBlobRoot: "data/sqlite/remember.db"},
		"bad duration":         {EnvReadTimeout: "later"},
		"zero duration":        {EnvShutdownTimeout: "0s"},
		"negative duration":    {EnvDatabaseBusy: "-1s"},
		"sub-millisecond busy": {EnvDatabaseBusy: "500us"},
		"header exceeds read":  {EnvReadHeaderTimeout: "20s", EnvReadTimeout: "10s"},
	}
	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(func(key string) (string, bool) {
				value, ok := values[key]
				return value, ok
			})
			if err == nil {
				t.Fatal("Load() accepted invalid configuration")
			}
			for _, value := range values {
				if strings.Contains(err.Error(), "remember.db") && strings.Contains(value, "secret") {
					t.Error("error leaked sensitive value")
				}
			}
		})
	}
}
