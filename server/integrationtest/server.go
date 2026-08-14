// Package integrationtest composes the real Remember HTTP stack for cross-module tests.
package integrationtest

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"time"

	"github.com/faulander/remember/server/internal/blob"
	"github.com/faulander/remember/server/internal/database"
	"github.com/faulander/remember/server/internal/httpapi"
	"github.com/faulander/remember/server/internal/identity"
	"github.com/faulander/remember/server/internal/session"
	synccore "github.com/faulander/remember/server/internal/sync"
	"github.com/faulander/remember/server/internal/verificationtoken"
	"github.com/google/uuid"
)

type Server struct {
	URL      string
	db       *sql.DB
	blobs    *blob.Repository
	http     *httptest.Server
	identity *identity.Service
}

func New(ctx context.Context, root string) (*Server, error) {
	db, err := database.Open(ctx, filepath.Join(root, "remember.db"), time.Second)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Server, error) { db.Close(); return nil, err }
	if err := database.Migrate(ctx, db); err != nil {
		return fail(err)
	}
	blobs, err := blob.Open(db, filepath.Join(root, "blobs"), filepath.Join(root, "staging"))
	if err != nil {
		return fail(err)
	}
	tokenCodec, err := verificationtoken.NewCodec(make([]byte, verificationtoken.KeySize))
	if err != nil {
		blobs.Close()
		return fail(err)
	}
	ident, err := identity.NewProductionService(db, tokenCodec)
	if err != nil {
		blobs.Close()
		return fail(err)
	}
	sessions, err := session.NewProductionService(db, ident)
	if err != nil {
		blobs.Close()
		return fail(err)
	}
	syncService, err := synccore.NewService(db, nil)
	if err != nil {
		blobs.Close()
		return fail(err)
	}
	state := &httpapi.State{}
	handler, err := httpapi.New(db, state, slog.New(slog.DiscardHandler), httpapi.Dependencies{Sessions: sessions, BlobForUser: func(userID uuid.UUID) (httpapi.BlobUserService, error) { return blobs.ForUser(userID) }, SyncForActor: func(userID, deviceID uuid.UUID) (httpapi.SyncActorService, error) {
		return syncService.ForActor(userID, deviceID)
	}})
	if err != nil {
		blobs.Close()
		return fail(err)
	}
	httpServer := httptest.NewServer(handler)
	state.MarkReady()
	return &Server{URL: httpServer.URL, db: db, blobs: blobs, http: httpServer, identity: ident}, nil
}

func (s *Server) CreateVerifiedUser(ctx context.Context, email, password string) error {
	if s == nil || s.identity == nil {
		return errors.New("integration server closed")
	}
	registration, err := s.identity.Register(ctx, email, password)
	if err != nil {
		return err
	}
	if !registration.Created {
		return errors.New("integration user already exists")
	}
	return s.identity.VerifyEmail(ctx, registration.VerificationToken)
}

func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	if s.http != nil {
		s.http.Close()
		s.http = nil
	}
	var result error
	if s.blobs != nil {
		result = s.blobs.Close()
		s.blobs = nil
	}
	if s.db != nil {
		result = errors.Join(result, s.db.Close())
		s.db = nil
	}
	s.identity = nil
	return result
}
