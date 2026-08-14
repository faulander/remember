// Package app composes the Remember server process.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/faulander/remember/server/internal/blob"
	"github.com/faulander/remember/server/internal/config"
	"github.com/faulander/remember/server/internal/database"
	"github.com/faulander/remember/server/internal/emaildelivery"
	"github.com/faulander/remember/server/internal/httpapi"
	"github.com/faulander/remember/server/internal/identity"
	"github.com/faulander/remember/server/internal/session"
	synccore "github.com/faulander/remember/server/internal/sync"
	"github.com/faulander/remember/server/internal/verificationtoken"
	"github.com/google/uuid"
)

// Run binds the configured listener and serves until cancellation or failure.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	return Serve(ctx, cfg, logger, listener)
}

// Serve runs against an existing listener, enabling deterministic lifecycle
// tests without weakening production configuration.
func Serve(ctx context.Context, cfg config.Config, logger *slog.Logger, listener net.Listener) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	db, err := database.Open(ctx, cfg.DatabasePath, cfg.DatabaseBusy)
	if err != nil {
		listener.Close()
		return err
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		listener.Close()
		return err
	}
	blobs, err := blob.OpenWithQuota(db, cfg.BlobRoot, cfg.StagingPath, cfg.UserBlobQuotaBytes)
	if err != nil {
		listener.Close()
		return fmt.Errorf("open blob repository: %w", err)
	}
	defer blobs.Close()
	recovery, err := blobs.RecoverStaging(ctx)
	if err != nil {
		listener.Close()
		return fmt.Errorf("recover blob staging: %w", err)
	}
	if recovery.Removed > 0 {
		logger.Info("blob_staging_recovered", "event_code", "BLOB_STAGING_RECOVERED", "count", recovery.Removed)
	}
	audit, err := blobs.Audit(ctx)
	if err != nil {
		listener.Close()
		return fmt.Errorf("audit blob repository: %w", err)
	}
	if audit.Missing > 0 || audit.Corrupt > 0 || audit.Malformed > 0 || audit.Symlinks > 0 {
		listener.Close()
		return fmt.Errorf("blob repository integrity check failed")
	}
	if audit.Orphans > 0 {
		logger.Warn("blob_orphans_detected", "event_code", "BLOB_ORPHANS_DETECTED", "count", audit.Orphans)
	}

	var verificationTokens *verificationtoken.Codec
	if cfg.EmailDeliveryEnabled() {
		verificationTokens, err = verificationtoken.NewCodec(cfg.EmailTokenKey)
		if err != nil {
			listener.Close()
			return fmt.Errorf("open email verification token seal: %w", err)
		}
	}
	identityService, err := identity.NewProductionService(db, verificationTokens)
	if err != nil {
		listener.Close()
		return fmt.Errorf("open identity service: %w", err)
	}
	sessionService, err := session.NewProductionService(db, identityService)
	if err != nil {
		listener.Close()
		return fmt.Errorf("open session service: %w", err)
	}
	var verificationDispatcher *emaildelivery.Dispatcher
	if cfg.EmailDeliveryEnabled() {
		sender, err := emaildelivery.NewSMTPSender(emaildelivery.SMTPConfig{
			Address: cfg.SMTPAddress, Username: cfg.SMTPUsername, Password: cfg.SMTPPassword,
			From: cfg.SMTPFrom, Timeout: cfg.SMTPTimeout,
		})
		if err != nil {
			listener.Close()
			return fmt.Errorf("open email verification sender: %w", err)
		}
		verificationDispatcher, err = emaildelivery.NewDispatcher(db, sender, nil, verificationTokens)
		if err != nil {
			listener.Close()
			return fmt.Errorf("open email verification dispatcher: %w", err)
		}
		go verificationDispatcher.Run(ctx, logger)
	}
	syncService, err := synccore.NewService(db, nil)
	if err != nil {
		listener.Close()
		return fmt.Errorf("open sync service: %w", err)
	}
	state := &httpapi.State{}
	handler, err := httpapi.New(db, state, logger, httpapi.Dependencies{
		Identity:            identityService,
		RegistrationEnabled: verificationDispatcher != nil,
		Sessions:            sessionService,
		BlobForUser: func(userID uuid.UUID) (httpapi.BlobUserService, error) {
			return blobs.ForUser(userID)
		},
		SyncForActor: func(userID, deviceID uuid.UUID) (httpapi.SyncActorService, error) {
			return syncService.ForActor(userID, deviceID)
		},
	})
	if err != nil {
		listener.Close()
		return fmt.Errorf("open HTTP API: %w", err)
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    1 << 20,
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	state.MarkReady()
	logger.Info("server_ready", "event_code", "SERVER_READY")

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-serveResult:
	}
	state.MarkDraining()
	logger.Info("server_draining", "event_code", "SERVER_DRAINING")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	if serveErr == nil {
		select {
		case serveErr = <-serveResult:
		case <-shutdownCtx.Done():
			if shutdownErr == nil {
				shutdownErr = shutdownCtx.Err()
			}
		}
	}
	if shutdownErr != nil {
		_ = server.Close()
		return fmt.Errorf("shutdown server: %w", shutdownErr)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP: %w", serveErr)
	}
	logger.Info("server_stopped", "event_code", "SERVER_STOPPED")
	return nil
}
