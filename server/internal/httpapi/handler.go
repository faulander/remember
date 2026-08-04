// Package httpapi contains the server's bounded HTTP surface.
package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/faulander/remember/server/internal/blob"
	"github.com/faulander/remember/server/internal/database"
	"github.com/faulander/remember/server/internal/identity"
	"github.com/faulander/remember/server/internal/session"
	synccore "github.com/faulander/remember/server/internal/sync"
	"github.com/google/uuid"
)

const (
	maxJSONBodyBytes   int64 = 16 << 10
	maxConcurrentBlobs       = 4
	maxConcurrentSync        = 8
	maxSyncPullLimit         = 500
)

// State controls whether the service may receive new work.
type State struct{ ready atomic.Bool }

func (s *State) MarkReady()    { s.ready.Store(true) }
func (s *State) MarkDraining() { s.ready.Store(false) }
func (s *State) Ready() bool   { return s.ready.Load() }

// SessionService is the only authority available to auth HTTP handlers. The
// raw database remains scoped to readiness checks.
type SessionService interface {
	Login(context.Context, string, string, string) (session.LoginResult, error)
	AuthenticateAccess(context.Context, string) (session.Principal, error)
	Refresh(context.Context, string) (session.Tokens, error)
	ListForUser(context.Context, string) ([]session.SessionInfo, error)
	RenameDevice(context.Context, string, uuid.UUID, string) error
	RevokeSession(context.Context, string, uuid.UUID) error
	RevokeDevice(context.Context, string, uuid.UUID) error
}

// Dependencies are explicit and injectable for bounded transport tests.
type BlobUserService interface {
	Put(context.Context, [sha256.Size]byte, io.Reader) (blob.PutResult, error)
	Get(context.Context, [sha256.Size]byte) ([]byte, error)
}

type BlobForUser func(uuid.UUID) (BlobUserService, error)

type SyncActorService interface {
	Submit(context.Context, synccore.Mutation) (synccore.SubmitResult, error)
	Pull(context.Context, uint64, int) (synccore.PullResult, error)
}

type SyncForActor func(uuid.UUID, uuid.UUID) (SyncActorService, error)

type Dependencies struct {
	Sessions     SessionService
	BlobForUser  BlobForUser
	SyncForActor SyncForActor
	Clock        Clock
}

// New returns the bounded public API.
func New(db *sql.DB, state *State, logger *slog.Logger, dependencies Dependencies) (http.Handler, error) {
	if db == nil || state == nil || dependencies.Sessions == nil || dependencies.BlobForUser == nil || dependencies.SyncForActor == nil {
		return nil, errors.New("http API dependency is nil")
	}
	if dependencies.Clock == nil {
		dependencies.Clock = wallClock{}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	api := &handler{
		db: db, state: state, sessions: dependencies.Sessions, blobForUser: dependencies.BlobForUser,
		syncForActor: dependencies.SyncForActor,
		limits:       newAbuseLimiters(dependencies.Clock), loginSlots: make(chan struct{}, maxConcurrentLogin),
		blobSlots: make(chan struct{}, maxConcurrentBlobs), syncSlots: make(chan struct{}, maxConcurrentSync),
	}
	return requestLog(logger, securityHeaders(api)), nil
}

type handler struct {
	db           *sql.DB
	state        *State
	sessions     SessionService
	limits       *abuseLimiters
	loginSlots   chan struct{}
	blobForUser  BlobForUser
	blobSlots    chan struct{}
	syncForActor SyncForActor
	syncSlots    chan struct{}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	blobRoute := onePathSegment(r.URL.Path, "/v1/blobs/")
	syncChangesRoute := r.URL.Path == "/v1/sync/changes"
	if r.URL.RawQuery != "" && !blobRoute && !syncChangesRoute {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	switch {
	case r.URL.Path == "/healthz":
		h.probe(w, r, false)
	case r.URL.Path == "/readyz":
		h.probe(w, r, true)
	case r.URL.Path == "/v1/auth/login":
		h.login(w, r)
	case r.URL.Path == "/v1/auth/refresh":
		h.refresh(w, r)
	case r.URL.Path == "/v1/auth/logout":
		h.logout(w, r)
	case r.URL.Path == "/v1/sessions":
		h.listSessions(w, r)
	case onePathSegment(r.URL.Path, "/v1/devices/"):
		h.device(w, r, strings.TrimPrefix(r.URL.Path, "/v1/devices/"))
	case onePathSegment(r.URL.Path, "/v1/sessions/"):
		h.session(w, r, strings.TrimPrefix(r.URL.Path, "/v1/sessions/"))
	case blobRoute:
		h.blob(w, r, strings.TrimPrefix(r.URL.Path, "/v1/blobs/"))
	case r.URL.Path == "/v1/sync/operations":
		h.submitSync(w, r)
	case syncChangesRoute:
		h.pullSync(w, r)
	default:
		writeAPIError(w, r, http.StatusNotFound, "not_found")
	}
}

func (h *handler) probe(w http.ResponseWriter, r *http.Request, readiness bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, r, "GET, HEAD")
		return
	}
	if !readiness {
		writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if !h.state.Ready() {
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 750*time.Millisecond)
	defer cancel()
	if err := database.Ready(ctx, h.db); err != nil {
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	var request struct {
		Email      string `json:"email"`
		Password   string `json:"password"`
		DeviceName string `json:"device_name"`
	}
	if err := decodeStrictJSON(w, r, &request); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	select {
	case h.loginSlots <- struct{}{}:
		defer func() { <-h.loginSlots }()
	default:
		writeRateLimited(w, r, time.Second)
		return
	}
	if allowed, retry := h.limits.allowLogin(request.Email); !allowed {
		writeRateLimited(w, r, retry)
		return
	}
	result, err := h.sessions.Login(r.Context(), request.Email, request.Password, request.DeviceName)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrInvalidCredentials), errors.Is(err, session.ErrUnauthenticated):
			writeAPIError(w, r, http.StatusUnauthorized, "invalid_credentials")
		case errors.Is(err, session.ErrInvalidInput):
			writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		default:
			writeAPIError(w, r, http.StatusInternalServerError, "internal_error")
		}
		return
	}
	writeJSON(w, r, http.StatusOK, loginResponse(result))
}

func (h *handler) refresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	var request struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := decodeStrictJSON(w, r, &request); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	if allowed, retry := h.limits.allowRefresh(request.RefreshToken); !allowed {
		writeRateLimited(w, r, retry)
		return
	}
	tokens, err := h.sessions.Refresh(r.Context(), request.RefreshToken)
	if err != nil {
		if errors.Is(err, session.ErrUnauthenticated) {
			writeAPIError(w, r, http.StatusUnauthorized, "invalid_session")
		} else {
			writeAPIError(w, r, http.StatusInternalServerError, "internal_error")
		}
		return
	}
	writeJSON(w, r, http.StatusOK, tokenResponse(tokens))
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	access, principal, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if err := requireEmptyBody(w, r); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.sessions.RevokeSession(r.Context(), access, principal.SessionID); err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "revoked"})
}

func (h *handler) listSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	access, _, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if err := requireEmptyBody(w, r); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	items, err := h.sessions.ListForUser(r.Context(), access)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	response := make([]sessionInfoResponse, 0, len(items))
	for _, item := range items {
		response = append(response, mapSessionInfo(item))
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"sessions": response})
}

func (h *handler) device(w http.ResponseWriter, r *http.Request, rawID string) {
	if r.Method != http.MethodPatch && r.Method != http.MethodDelete {
		methodNotAllowed(w, r, "PATCH, DELETE")
		return
	}
	access, _, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	id, err := parseUUIDv7(rawID)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	if r.Method == http.MethodPatch {
		var request struct {
			DisplayName string `json:"display_name"`
		}
		if err := decodeStrictJSON(w, r, &request); err != nil {
			writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
		err = h.sessions.RenameDevice(r.Context(), access, id, request.DisplayName)
	} else {
		if err := requireEmptyBody(w, r); err != nil {
			writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
		err = h.sessions.RevokeDevice(r.Context(), access, id)
	}
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) session(w http.ResponseWriter, r *http.Request, rawID string) {
	if r.Method != http.MethodDelete {
		methodNotAllowed(w, r, http.MethodDelete)
		return
	}
	access, _, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	id, err := parseUUIDv7(rawID)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := requireEmptyBody(w, r); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := h.sessions.RevokeSession(r.Context(), access, id); err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

type syncMutationRequest struct {
	OperationID  string  `json:"operation_id"`
	Mutation     string  `json:"mutation"`
	ObjectID     string  `json:"object_id"`
	ObjectType   string  `json:"object_type"`
	BaseRevision uint64  `json:"base_revision"`
	ParentID     *string `json:"parent_id"`
	Name         string  `json:"name"`
	BlobHash     *string `json:"blob_hash"`
}

type syncSubmitResponse struct {
	Accepted bool    `json:"accepted"`
	Conflict *string `json:"conflict"`
	Revision *uint64 `json:"revision"`
	Cursor   *uint64 `json:"cursor"`
}

type syncChangeResponse struct {
	Cursor      uint64  `json:"cursor"`
	Mutation    string  `json:"mutation"`
	OperationID string  `json:"operation_id"`
	ObjectID    string  `json:"object_id"`
	ObjectType  string  `json:"object_type"`
	Revision    uint64  `json:"revision"`
	ParentID    *string `json:"parent_id"`
	Name        string  `json:"name"`
	BlobHash    *string `json:"blob_hash"`
	Deleted     bool    `json:"deleted"`
}

func (h *handler) submitSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}
	_, principal, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	var request syncMutationRequest
	if err := decodeStrictJSONFields(w, r, &request,
		"operation_id", "mutation", "object_id", "object_type", "base_revision", "parent_id", "name", "blob_hash"); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	mutation, err := mapSyncMutation(request)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	actor, err := h.syncForActor(principal.UserID, principal.DeviceID)
	if err != nil {
		writeAPIError(w, r, http.StatusInternalServerError, "internal_error")
		return
	}
	if !h.acquireSyncSlot(w, r) {
		return
	}
	defer func() { <-h.syncSlots }()
	result, err := actor.Submit(r.Context(), mutation)
	if err != nil {
		h.writeSyncError(w, r, err)
		return
	}
	response := syncSubmitResponse{Accepted: result.Accepted}
	if result.Accepted {
		revision, cursor := result.Revision, result.Cursor
		response.Revision, response.Cursor = &revision, &cursor
	} else {
		conflict := string(result.Conflict)
		response.Conflict = &conflict
	}
	writeJSON(w, r, http.StatusOK, response)
}

func (h *handler) pullSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	_, principal, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	after, limit, err := parseSyncQuery(r.URL.RawQuery)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := requireEmptyBody(w, r); err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	actor, err := h.syncForActor(principal.UserID, principal.DeviceID)
	if err != nil {
		writeAPIError(w, r, http.StatusInternalServerError, "internal_error")
		return
	}
	if !h.acquireSyncSlot(w, r) {
		return
	}
	defer func() { <-h.syncSlots }()
	result, err := actor.Pull(r.Context(), after, limit)
	if err != nil {
		h.writeSyncError(w, r, err)
		return
	}
	changes := make([]syncChangeResponse, 0, len(result.Changes))
	for _, item := range result.Changes {
		change := syncChangeResponse{
			Cursor: item.Cursor, Mutation: string(item.Mutation), OperationID: item.OperationID.String(),
			ObjectID: item.ObjectID.String(), ObjectType: string(item.ObjectType), Revision: item.Revision,
			Name: item.Name, Deleted: item.Deleted,
		}
		if item.ParentID != nil {
			parent := item.ParentID.String()
			change.ParentID = &parent
		}
		if item.BlobHash != nil {
			hash := hex.EncodeToString(item.BlobHash)
			change.BlobHash = &hash
		}
		changes = append(changes, change)
	}
	writeJSON(w, r, http.StatusOK, map[string]any{
		"changes": changes, "has_more": result.HasMore, "next_cursor": result.NextCursor,
	})
}

func mapSyncMutation(request syncMutationRequest) (synccore.Mutation, error) {
	operationID, err := parseUUIDv7(request.OperationID)
	if err != nil {
		return synccore.Mutation{}, err
	}
	objectID, err := parseUUIDv7(request.ObjectID)
	if err != nil {
		return synccore.Mutation{}, err
	}
	kind := synccore.MutationKind(request.Mutation)
	if kind != synccore.MutationCreate && kind != synccore.MutationUpdate && kind != synccore.MutationMove && kind != synccore.MutationDelete {
		return synccore.Mutation{}, synccore.ErrInvalidInput
	}
	objectType := synccore.ObjectType(request.ObjectType)
	if objectType != synccore.ObjectNote && objectType != synccore.ObjectFolder {
		return synccore.Mutation{}, synccore.ErrInvalidInput
	}
	if request.BaseRevision > math.MaxInt64 {
		return synccore.Mutation{}, synccore.ErrInvalidInput
	}
	result := synccore.Mutation{
		OperationID: operationID, Kind: kind, ObjectID: objectID, ObjectType: objectType,
		BaseRevision: request.BaseRevision, Name: request.Name,
	}
	if request.ParentID != nil {
		parent, err := parseUUIDv7(*request.ParentID)
		if err != nil {
			return synccore.Mutation{}, err
		}
		result.ParentID = &parent
	}
	if request.BlobHash != nil {
		hash, err := parseBlobHash(*request.BlobHash)
		if err != nil {
			return synccore.Mutation{}, err
		}
		result.BlobHash = append([]byte(nil), hash[:]...)
	}
	return result, nil
}

func parseSyncQuery(raw string) (uint64, int, error) {
	if raw == "" {
		return 0, 0, nil
	}
	values := make(map[string]string, 2)
	for _, part := range strings.Split(raw, "&") {
		key, value, ok := strings.Cut(part, "=")
		if !ok || value == "" || (key != "after" && key != "limit") {
			return 0, 0, synccore.ErrInvalidInput
		}
		if _, duplicate := values[key]; duplicate {
			return 0, 0, synccore.ErrInvalidInput
		}
		values[key] = value
	}
	var after uint64
	var err error
	if rawAfter, ok := values["after"]; ok {
		after, err = parseCanonicalUint(rawAfter)
		if err != nil || after > math.MaxInt64 {
			return 0, 0, synccore.ErrInvalidInput
		}
	}
	var limit int
	if rawLimit, ok := values["limit"]; ok {
		parsed, err := parseCanonicalUint(rawLimit)
		if err != nil || parsed > maxSyncPullLimit {
			return 0, 0, synccore.ErrInvalidInput
		}
		limit = int(parsed)
	}
	return after, limit, nil
}

func parseCanonicalUint(raw string) (uint64, error) {
	if raw == "" {
		return 0, synccore.ErrInvalidInput
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || strconv.FormatUint(value, 10) != raw {
		return 0, synccore.ErrInvalidInput
	}
	return value, nil
}

func (h *handler) acquireSyncSlot(w http.ResponseWriter, r *http.Request) bool {
	select {
	case h.syncSlots <- struct{}{}:
		return true
	default:
		writeRateLimited(w, r, time.Second)
		return false
	}
}

func (h *handler) writeSyncError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, synccore.ErrInvalidInput):
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, synccore.ErrInactiveActor):
		writeAPIError(w, r, http.StatusUnauthorized, "invalid_session")
	case errors.Is(err, synccore.ErrBlobUnavailable):
		writeAPIError(w, r, http.StatusConflict, "blob_unavailable")
	case errors.Is(err, synccore.ErrOperationReplayMismatch):
		writeAPIError(w, r, http.StatusConflict, "operation_replay_mismatch")
	default:
		writeAPIError(w, r, http.StatusInternalServerError, "internal_error")
	}
}

func (h *handler) blob(w http.ResponseWriter, r *http.Request, rawHash string) {
	if r.Method != http.MethodPut && r.Method != http.MethodGet {
		methodNotAllowed(w, r, "PUT, GET")
		return
	}
	_, principal, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	hash, err := parseBlobHash(rawHash)
	if err != nil {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	if hasUnsupportedBlobHeaders(r) {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	userBlobs, err := h.blobForUser(principal.UserID)
	if err != nil {
		writeAPIError(w, r, http.StatusInternalServerError, "internal_error")
		return
	}
	if r.Method == http.MethodGet {
		if err := requireEmptyBody(w, r); err != nil {
			writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
			return
		}
		if !h.acquireBlobSlot(w, r) {
			return
		}
		defer func() { <-h.blobSlots }()
		content, err := userBlobs.Get(r.Context(), hash)
		if err != nil {
			h.writeBlobError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
		return
	}

	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	mediaType, parameters, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/octet-stream" || len(parameters) != 0 || r.ContentLength < 0 {
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
		return
	}
	if r.ContentLength > blob.MaxBlobBytes {
		writeAPIError(w, r, http.StatusRequestEntityTooLarge, "blob_too_large")
		return
	}
	if !h.acquireBlobSlot(w, r) {
		return
	}
	defer func() { <-h.blobSlots }()
	r.Body = http.MaxBytesReader(w, r.Body, blob.MaxBlobBytes+1)
	result, err := userBlobs.Put(r.Context(), hash, &exactLengthReader{source: r.Body, remaining: r.ContentLength})
	if err != nil {
		h.writeBlobError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"hash": hex.EncodeToString(result.Hash[:]), "size": result.Size})
}

func (h *handler) acquireBlobSlot(w http.ResponseWriter, r *http.Request) bool {
	select {
	case h.blobSlots <- struct{}{}:
		return true
	default:
		writeRateLimited(w, r, time.Second)
		return false
	}
}

var errBodyLength = errors.New("request body length mismatch")

type exactLengthReader struct {
	source    io.Reader
	remaining int64
	finished  bool
}

func (r *exactLengthReader) Read(buffer []byte) (int, error) {
	if r.finished {
		return 0, io.EOF
	}
	if r.remaining > 0 {
		if int64(len(buffer)) > r.remaining {
			buffer = buffer[:r.remaining]
		}
		count, err := r.source.Read(buffer)
		r.remaining -= int64(count)
		if (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) && r.remaining > 0 {
			return count, errBodyLength
		}
		return count, err
	}
	var extra [1]byte
	count, err := r.source.Read(extra[:])
	if count != 0 || err == nil {
		return 0, errBodyLength
	}
	if err != io.EOF {
		return 0, err
	}
	r.finished = true
	return 0, io.EOF
}

func parseBlobHash(raw string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	if len(raw) != sha256.Size*2 || raw != strings.ToLower(raw) {
		return result, errors.New("invalid blob hash")
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil || len(decoded) != sha256.Size {
		return result, errors.New("invalid blob hash")
	}
	copy(result[:], decoded)
	return result, nil
}

func hasUnsupportedBlobHeaders(r *http.Request) bool {
	for _, name := range []string{"Content-Encoding", "Range", "Content-Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
		if len(r.Header.Values(name)) != 0 {
			return true
		}
	}
	return false
}

func (h *handler) writeBlobError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, blob.ErrUnavailable):
		writeAPIError(w, r, http.StatusNotFound, "blob_not_found")
	case errors.Is(err, blob.ErrQuotaExceeded):
		writeAPIError(w, r, http.StatusRequestEntityTooLarge, "quota_exceeded")
	case errors.Is(err, blob.ErrTooLarge):
		writeAPIError(w, r, http.StatusRequestEntityTooLarge, "blob_too_large")
	case errors.Is(err, blob.ErrHashMismatch):
		writeAPIError(w, r, http.StatusUnprocessableEntity, "hash_mismatch")
	case errors.Is(err, errBodyLength):
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
	default:
		writeAPIError(w, r, http.StatusInternalServerError, "internal_error")
	}
}

func (h *handler) authenticate(w http.ResponseWriter, r *http.Request) (string, session.Principal, bool) {
	access, err := bearerToken(r)
	if err != nil {
		writeAPIError(w, r, http.StatusUnauthorized, "invalid_session")
		return "", session.Principal{}, false
	}
	principal, err := h.sessions.AuthenticateAccess(r.Context(), access)
	if err != nil {
		if errors.Is(err, session.ErrUnauthenticated) {
			writeAPIError(w, r, http.StatusUnauthorized, "invalid_session")
		} else {
			writeAPIError(w, r, http.StatusInternalServerError, "internal_error")
		}
		return "", session.Principal{}, false
	}
	return access, principal, true
}

func (h *handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, session.ErrUnauthenticated):
		writeAPIError(w, r, http.StatusUnauthorized, "invalid_session")
	case errors.Is(err, session.ErrInvalidInput):
		writeAPIError(w, r, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, session.ErrNotFound):
		writeAPIError(w, r, http.StatusNotFound, "not_found")
	default:
		writeAPIError(w, r, http.StatusInternalServerError, "internal_error")
	}
}

func decodeStrictJSON(w http.ResponseWriter, r *http.Request, target any) error {
	return decodeStrictJSONFields(w, r, target)
}

func decodeStrictJSONFields(w http.ResponseWriter, r *http.Request, target any, required ...string) error {
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return errors.New("invalid JSON media type")
	}
	mediaType, parameters, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/json" || len(parameters) != 0 {
		return errors.New("invalid JSON media type")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	content, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if err := rejectDuplicateJSONKeys(content); err != nil {
		return err
	}
	if len(required) != 0 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(content, &fields); err != nil || fields == nil {
			return errors.New("JSON object required")
		}
		for _, name := range required {
			if _, ok := fields[name]; !ok {
				return errors.New("required JSON field missing")
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func rejectDuplicateJSONKeys(content []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, structured := token.(json.Delim)
	if !structured {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("invalid JSON object key")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("duplicate JSON object key")
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}

func requireEmptyBody(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	content, err := io.ReadAll(r.Body)
	if err != nil || len(content) != 0 {
		return session.ErrInvalidInput
	}
	return nil
}

func bearerToken(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > 128 || !strings.HasPrefix(values[0], "Bearer ") {
		return "", session.ErrUnauthenticated
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if len(token) != 43 || strings.ContainsAny(token, " \t\r\n,") {
		return "", session.ErrUnauthenticated
	}
	return token, nil
}

func onePathSegment(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	remainder := strings.TrimPrefix(path, prefix)
	return remainder != "" && !strings.Contains(remainder, "/")
}

func parseUUIDv7(raw string) (uuid.UUID, error) {
	if len(raw) != 36 || strings.Contains(raw, "/") {
		return uuid.Nil, session.ErrInvalidInput
	}
	id, err := uuid.Parse(raw)
	if err != nil || id.String() != raw || id.Version() != 7 || id.Variant() != uuid.RFC4122 {
		return uuid.Nil, session.ErrInvalidInput
	}
	return id, nil
}

type tokenPairResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	AccessExpiresAt  string `json:"access_expires_at"`
	RefreshExpiresAt string `json:"refresh_expires_at"`
}

type loginResultResponse struct {
	Principal principalJSON     `json:"principal"`
	Tokens    tokenPairResponse `json:"tokens"`
}

type principalJSON struct {
	UserID    string `json:"user_id"`
	DeviceID  string `json:"device_id"`
	SessionID string `json:"session_id"`
}

type sessionInfoResponse struct {
	SessionID  string  `json:"session_id"`
	DeviceID   string  `json:"device_id"`
	DeviceName string  `json:"device_name"`
	Status     string  `json:"status"`
	CreatedAt  string  `json:"created_at"`
	ExpiresAt  string  `json:"expires_at"`
	RevokedAt  *string `json:"revoked_at"`
	Current    bool    `json:"current"`
}

func loginResponse(result session.LoginResult) loginResultResponse {
	return loginResultResponse{
		Principal: principalJSON{
			UserID: result.Principal.UserID.String(), DeviceID: result.Principal.DeviceID.String(), SessionID: result.Principal.SessionID.String(),
		},
		Tokens: tokenResponse(result.Tokens),
	}
}

func tokenResponse(tokens session.Tokens) tokenPairResponse {
	return tokenPairResponse{
		AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken,
		AccessExpiresAt:  tokens.AccessExpiresAt.UTC().Format(time.RFC3339Nano),
		RefreshExpiresAt: tokens.RefreshExpiresAt.UTC().Format(time.RFC3339Nano),
	}
}

func mapSessionInfo(item session.SessionInfo) sessionInfoResponse {
	result := sessionInfoResponse{
		SessionID: item.SessionID.String(), DeviceID: item.DeviceID.String(), DeviceName: item.DeviceName,
		Status: item.Status, CreatedAt: item.CreatedAt.UTC().Format(time.RFC3339Nano),
		ExpiresAt: item.ExpiresAt.UTC().Format(time.RFC3339Nano), Current: item.Current,
	}
	if item.RevokedAt != nil {
		value := item.RevokedAt.UTC().Format(time.RFC3339Nano)
		result.RevokedAt = &value
	}
	return result
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	writeAPIError(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
}

func writeRateLimited(w http.ResponseWriter, r *http.Request, retry time.Duration) {
	seconds := int64((retry + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	writeAPIError(w, r, http.StatusTooManyRequests, "rate_limited")
}

func writeAPIError(w http.ResponseWriter, r *http.Request, status int, code string) {
	writeJSON(w, r, status, map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(value)
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func requestLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := newRequestID()
		w.Header().Set("X-Request-ID", requestID)
		capture := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(capture, r)
		logger.Info("http_request",
			"event_code", "HTTP_REQUEST",
			"request_id", requestID,
			"method", r.Method,
			"route", knownRoute(r.URL.Path),
			"status", capture.status,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func newRequestID() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "request-id-unavailable"
	}
	return hex.EncodeToString(value[:])
}

func knownRoute(path string) string {
	switch path {
	case "/healthz", "/readyz", "/v1/auth/login", "/v1/auth/refresh", "/v1/auth/logout", "/v1/sessions", "/v1/sync/operations", "/v1/sync/changes":
		return path
	default:
		switch {
		case onePathSegment(path, "/v1/devices/"):
			return "/v1/devices/{id}"
		case onePathSegment(path, "/v1/sessions/"):
			return "/v1/sessions/{id}"
		case onePathSegment(path, "/v1/blobs/"):
			return "/v1/blobs/{hash}"
		default:
			return "unknown"
		}
	}
}
