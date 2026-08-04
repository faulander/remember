package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/faulander/remember/server/internal/config"
	"github.com/faulander/remember/server/internal/database"
)

func TestServeReadyPersistentAndGracefulShutdown(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	if err := os.MkdirAll(cfg.StagingPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StagingPath, ".upload-0123456789abcdef0123456789abcdef"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Serve(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), listener) }()

	waitForReady(t, "http://"+address+"/readyz")
	if _, err := os.Lstat(filepath.Join(cfg.StagingPath, ".upload-0123456789abcdef0123456789abcdef")); !os.IsNotExist(err) {
		t.Fatalf("startup did not recover staging: %v", err)
	}
	response, err := http.Get("http://" + address + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("health status = %d", response.StatusCode)
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Serve() shutdown error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve() did not stop within shutdown bound")
	}

	db, err := database.Open(context.Background(), cfg.DatabasePath, cfg.DatabaseBusy)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatalf("persistent database failed idempotent migration: %v", err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Errorf("migration count after restart = %d, want 4", count)
	}
}

func TestServeRefusesMissingRegisteredBlobBeforeReady(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	db, err := database.Open(context.Background(), cfg.DatabasePath, cfg.DatabaseBusy)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	hash := make([]byte, 32)
	if _, err := db.Exec("INSERT INTO content_blobs(hash,size_bytes,available,created_at_ms) VALUES(?,1,1,1)", hash); err != nil {
		t.Fatal(err)
	}
	db.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := Serve(context.Background(), cfg, nil, listener); err == nil {
		t.Fatal("Serve accepted missing registered blob")
	}
}

func TestServeReturnsListenerFailure(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Serve(context.Background(), cfg, nil, listener); err == nil {
		t.Fatal("Serve() accepted closed listener")
	}
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	cfg.DatabasePath = filepath.Join(data, "sqlite", "server.db")
	cfg.BlobRoot = filepath.Join(data, "blobs")
	cfg.StagingPath = filepath.Join(data, "staging")
	cfg.ShutdownTimeout = time.Second
	return cfg
}

func waitForReady(t *testing.T, url string) {
	t.Helper()
	client := &http.Client{Timeout: 250 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(url)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server did not become ready")
}
