package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreapp "github.com/faulander/remember/client/internal/app"
	"github.com/faulander/remember/client/internal/remotehttp"
	"github.com/faulander/remember/client/internal/sessionstore"
	"github.com/google/uuid"
)

func TestStateFromCoreExposesOnlyRenderableMetadata(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Note.md"), []byte("# Private body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	core, report, err := coreapp.Initialize(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()

	state, err := stateFromCore(context.Background(), core, report, 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	if state.Generation != 7 || state.Revision != 3 || state.Root != core.Root() {
		t.Errorf("state identity = %#v", state)
	}
	if len(state.Objects) != 1 || state.Objects[0].RelativePath != "Note.md" {
		t.Errorf("state objects = %#v", state.Objects)
	}
	if state.Objects[0].ID == "" || state.Objects[0].Type != "note" {
		t.Errorf("note DTO = %#v", state.Objects[0])
	}
}

func TestDesktopNoteLifecycleAndStateTagsWithoutBody(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	app := NewDesktopApp()
	app.emit = func(context.Context, string, ...interface{}) {}
	app.startup(context.Background())
	defer app.shutdown(context.Background())
	if _, err := app.InitializeRoot(root); err != nil {
		t.Fatal(err)
	}
	created, err := app.CreateNote("", "Private")
	if err != nil {
		t.Fatal(err)
	}
	created.Note.Body = "private body marker"
	created.Note.Tags = []string{"Secret"}
	saved, err := app.SaveNote(SaveNoteRequest{
		RelativePath: created.Note.RelativePath, ExpectedRevision: created.Note.Revision,
		Body: created.Note.Body, Tags: created.Note.Tags,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.State.Objects) != 1 || len(saved.State.Objects[0].Tags) != 1 || saved.State.Objects[0].Tags[0] != "Secret" {
		t.Fatalf("state tags = %#v", saved.State.Objects)
	}
	encoded, err := json.Marshal(saved.State)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private body marker") {
		t.Fatal("global ClientState exposed note body")
	}
	read, err := app.ReadNote("Private.md")
	if err != nil || read.Body != "private body marker" || read.ID != saved.Note.ID {
		t.Fatalf("ReadNote() = %#v, %v", read, err)
	}
	moved, err := app.MoveNote(MoveNoteRequest{
		RelativePath: read.RelativePath, ExpectedRevision: read.Revision, Name: "Renamed",
	})
	if err != nil || moved.Note.RelativePath != "Renamed.md" {
		t.Fatalf("MoveNote() = %#v, %v", moved, err)
	}
	final, err := app.DeleteNote(DeleteNoteRequest{RelativePath: moved.Note.RelativePath, ExpectedRevision: moved.Note.Revision})
	if err != nil || len(final.Objects) != 0 {
		t.Fatalf("DeleteNote() objects=%#v, %v", final.Objects, err)
	}
}

func TestDesktopLoginSyncAndLogout(t *testing.T) {
	root := t.TempDir()
	user, device, sessionID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	otherDevice, otherSession := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	pulls, logouts, renames, revokes, deviceRevokes := 0, 0, 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/login":
			var body map[string]string
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil || body["email"] != "person@example.com" || body["password"] != "secret" || body["device_name"] != "Desktop" {
				t.Errorf("login request = %#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"principal": map[string]string{"user_id": user.String(), "device_id": device.String(), "session_id": sessionID.String()},
				"tokens": map[string]string{
					"access_token": "access", "refresh_token": "refresh",
					"access_expires_at":  time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339Nano),
					"refresh_expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
				},
			})
		case "/v1/sessions":
			_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []map[string]any{
				{"session_id": sessionID.String(), "device_id": device.String(), "device_name": "Desktop", "status": "active", "created_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), "revoked_at": nil, "current": true},
				{"session_id": otherSession.String(), "device_id": otherDevice.String(), "device_name": "Notebook", "status": "active", "created_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), "revoked_at": nil, "current": false},
			}})
		case "/v1/devices/" + device.String():
			renames++
			var body map[string]string
			if r.Method != http.MethodPatch || json.NewDecoder(r.Body).Decode(&body) != nil || body["display_name"] != "Arbeitsplatz" {
				t.Errorf("rename request = %s %#v", r.Method, body)
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/devices/" + otherDevice.String():
			deviceRevokes++
			if r.Method != http.MethodDelete {
				t.Errorf("device revoke method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/sessions/" + otherSession.String():
			revokes++
			if r.Method != http.MethodDelete {
				t.Errorf("revoke method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/sync/changes":
			pulls++
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer access" || r.URL.Query().Get("after") != "0" {
				t.Errorf("pull request = %s %s auth=%q", r.Method, r.URL.String(), r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"changes":[],"has_more":false,"next_cursor":0}`))
		case "/v1/auth/logout":
			logouts++
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer access" {
				t.Errorf("logout request auth=%q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"status":"revoked"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	app := NewDesktopApp()
	credentials := &memoryCredentialStore{}
	app.credentials = credentials
	app.emit = func(context.Context, string, ...interface{}) {}
	app.startup(context.Background())
	defer app.shutdown(context.Background())
	if _, err := app.InitializeRoot(root); err != nil {
		t.Fatal(err)
	}
	view, err := app.Login(LoginRequest{ServerURL: server.URL, Email: "person@example.com", Password: "secret", DeviceName: "Desktop"})
	if err != nil || view.UserID != user.String() || view.DeviceID != device.String() || view.SessionID != sessionID.String() {
		t.Fatalf("Login() = %#v, %v", view, err)
	}
	if !credentials.exists || credentials.credential.RefreshToken != "refresh" || credentials.credential.ServerURL != server.URL {
		t.Fatalf("persisted credential = %#v, exists=%v", credentials.credential, credentials.exists)
	}
	sessions, err := app.ListSessions()
	if err != nil || len(sessions) != 2 || !sessions[0].Current || sessions[1].SessionID != otherSession.String() {
		t.Fatalf("ListSessions() = %#v, %v", sessions, err)
	}
	if err := app.RenameCurrentDevice("Arbeitsplatz"); err != nil {
		t.Fatal(err)
	}
	if err := app.RevokeSession(otherSession.String()); err != nil {
		t.Fatal(err)
	}
	if err := app.RevokeDevice(otherDevice.String()); err != nil {
		t.Fatal(err)
	}
	if renames != 1 || revokes != 1 || deviceRevokes != 1 {
		t.Fatalf("renames=%d revokes=%d deviceRevokes=%d", renames, revokes, deviceRevokes)
	}
	if _, err := app.SyncNow(); err != nil {
		t.Fatal(err)
	}
	if pulls != 1 {
		t.Fatalf("pulls = %d", pulls)
	}
	if err := app.Logout(); err != nil {
		t.Fatal(err)
	}
	if logouts != 1 {
		t.Fatalf("logouts = %d", logouts)
	}
	if credentials.exists {
		t.Fatal("credential retained after logout")
	}
	if _, err := app.SyncNow(); !errors.Is(err, remotehttp.ErrReauthRequired) {
		t.Fatalf("post-logout SyncNow() error = %v", err)
	}
}

func TestDesktopRestoresAndRotatesPersistedSession(t *testing.T) {
	user, device, sessionID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	refreshes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/refresh":
			refreshes++
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["refresh_token"] != "persisted-refresh" {
				t.Errorf("refresh body = %#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"access_token": "restored-access", "refresh_token": "rotated-refresh",
				"access_expires_at":  time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339Nano),
				"refresh_expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
			})
		case "/v1/auth/logout":
			if r.Header.Get("Authorization") != "Bearer restored-access" {
				t.Errorf("logout authorization = %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"status":"revoked"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	credentials := &memoryCredentialStore{exists: true, credential: sessionstore.Credential{
		ServerURL: server.URL, UserID: user.String(), DeviceID: device.String(), SessionID: sessionID.String(),
		RefreshToken: "persisted-refresh", RefreshExpiresAt: time.Now().Add(time.Hour),
	}}
	app := NewDesktopApp()
	app.credentials = credentials
	app.emit = func(context.Context, string, ...interface{}) {}
	app.startup(context.Background())
	defer app.shutdown(context.Background())

	view, err := app.RestoreSession()
	if err != nil || view == nil || view.SessionID != sessionID.String() {
		t.Fatalf("RestoreSession() = %#v, %v", view, err)
	}
	if refreshes != 1 || credentials.credential.RefreshToken != "rotated-refresh" {
		t.Fatalf("refreshes=%d credential=%#v", refreshes, credentials.credential)
	}
	replayed, err := app.RestoreSession()
	if err != nil || replayed == nil || refreshes != 1 {
		t.Fatalf("second RestoreSession() = %#v, %v; refreshes=%d", replayed, err, refreshes)
	}
	if err := app.Logout(); err != nil {
		t.Fatal(err)
	}
}

type memoryCredentialStore struct {
	credential sessionstore.Credential
	exists     bool
	err        error
}

func (s *memoryCredentialStore) Load() (sessionstore.Credential, error) {
	if s.err != nil {
		return sessionstore.Credential{}, s.err
	}
	if !s.exists {
		return sessionstore.Credential{}, sessionstore.ErrNotFound
	}
	return s.credential, nil
}

func (s *memoryCredentialStore) Save(credential sessionstore.Credential) error {
	if s.err != nil {
		return s.err
	}
	s.credential, s.exists = credential, true
	return nil
}

func (s *memoryCredentialStore) Delete() error {
	if s.err != nil {
		return s.err
	}
	s.credential, s.exists = sessionstore.Credential{}, false
	return nil
}

func TestEditableNotePathRejectsInvalidFolderAndNormalizesName(t *testing.T) {
	t.Parallel()
	path, title, err := editableNotePath("Folder", " Café.md ")
	if err != nil || path != "Folder/Café.md" || title != "Café" {
		t.Errorf("editableNotePath() = %q, %q, %v", path, title, err)
	}
	for _, folder := range []string{"../escape", ".remember", "/absolute"} {
		if _, _, err := editableNotePath(folder, "Note"); err == nil {
			t.Errorf("invalid folder %q accepted", folder)
		}
	}
}

func TestStateExposesBackendUnicodeTagKeys(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	core, _, err := coreapp.Initialize(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	if _, _, err := core.CreateNote(context.Background(), "Street.md", "body", []string{"Straße"}); err != nil {
		t.Fatal(err)
	}
	report, err := core.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := stateFromCore(context.Background(), core, report, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Objects) != 1 || len(state.Objects[0].TagKeys) != 1 || state.Objects[0].TagKeys[0] != "strasse" {
		t.Fatalf("tag state = %#v", state.Objects)
	}
	normalized, err := NewDesktopApp().NormalizeTags([]string{"Straße", "STRASSE"})
	if err != nil || len(normalized) != 1 || normalized[0] != "Straße" {
		t.Fatalf("NormalizeTags() = %#v, %v", normalized, err)
	}
}

func TestCreateFolderAndDefaultNoteNames(t *testing.T) {
	root := t.TempDir()
	app := NewDesktopApp()
	app.emit = func(context.Context, string, ...interface{}) {}
	app.startup(context.Background())
	defer app.shutdown(context.Background())
	if _, err := app.InitializeRoot(root); err != nil {
		t.Fatal(err)
	}
	folderState, err := app.CreateFolder("", "Projekte")
	if err != nil {
		t.Fatal(err)
	}
	if !hasObjectPath(folderState.Objects, "Projekte") {
		t.Fatalf("created folder missing from state: %#v", folderState.Objects)
	}
	first, err := app.CreateNote("Projekte", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := app.CreateNote("Projekte", "   ")
	if err != nil {
		t.Fatal(err)
	}
	if first.Note.RelativePath != "Projekte/Neue Notiz.md" || second.Note.RelativePath != "Projekte/Neue Notiz 2.md" {
		t.Errorf("default note paths = %q, %q", first.Note.RelativePath, second.Note.RelativePath)
	}
}

func hasObjectPath(objects []ObjectView, relative string) bool {
	for _, object := range objects {
		if object.RelativePath == relative {
			return true
		}
	}
	return false
}

func TestShutdownWaitsForOperationsAndRejectsNewOnes(t *testing.T) {
	t.Parallel()

	app := NewDesktopApp()
	app.startup(context.Background())
	_, done, err := app.beginOperation()
	if err != nil {
		t.Fatal(err)
	}

	shutdownStarted := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		close(shutdownStarted)
		app.shutdown(context.Background())
		close(shutdownDone)
	}()
	<-shutdownStarted
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned while an operation was active")
	case <-time.After(30 * time.Millisecond):
	}
	done()
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish after operation completed")
	}
	if _, _, err := app.beginOperation(); err == nil {
		t.Error("operation started after shutdown")
	}
}

func TestWatcherForwardingUsesMonotonicVersionsAndStopsBeforeCloseReturns(t *testing.T) {
	root := t.TempDir()
	app := NewDesktopApp()
	app.startup(context.Background())
	events := make(chan map[string]any, 32)
	app.emit = func(_ context.Context, name string, data ...interface{}) {
		if name != stateEvent || len(data) != 1 {
			t.Errorf("unexpected event %q %#v", name, data)
			return
		}
		event, ok := data[0].(map[string]any)
		if !ok {
			t.Errorf("unexpected event payload %#v", data[0])
			return
		}
		events <- event
	}

	initial, err := app.InitializeRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Generation != 1 || initial.Revision != 0 {
		t.Fatalf("initial version = %d/%d, want 1/0", initial.Generation, initial.Revision)
	}
	first := receiveDesktopEvent(t, events)
	if first["generation"] != uint64(1) {
		t.Errorf("event generation = %#v", first["generation"])
	}
	if revision, ok := first["revision"].(uint64); !ok || revision < 1 {
		t.Errorf("event revision = %#v, want >= 1", first["revision"])
	}

	if err := os.WriteFile(filepath.Join(root, "External.md"), []byte("Body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = receiveDesktopEvent(t, events)
	generation, err := app.CloseRoot()
	if err != nil {
		t.Fatal(err)
	}
	if generation != 2 {
		t.Errorf("close generation = %d, want 2", generation)
	}
	for len(events) > 0 {
		<-events
	}
	if err := os.WriteFile(filepath.Join(root, "AfterClose.md"), []byte("Body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		t.Fatalf("event arrived after CloseRoot returned: %#v", event)
	case <-time.After(100 * time.Millisecond):
	}
	app.shutdown(context.Background())
}

func receiveDesktopEvent(t *testing.T, events <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for desktop event")
		return nil
	}
}

func TestCloseRootAdvancesGeneration(t *testing.T) {
	t.Parallel()

	app := NewDesktopApp()
	app.startup(context.Background())
	generation, err := app.CloseRoot()
	if err != nil {
		t.Fatal(err)
	}
	if generation != 1 {
		t.Errorf("generation = %d, want 1", generation)
	}
	app.shutdown(context.Background())
}
