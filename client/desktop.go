package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	coreapp "github.com/faulander/remember/client/internal/app"
	"github.com/faulander/remember/client/internal/frontmatter"
	"github.com/faulander/remember/client/internal/localindex"
	"github.com/faulander/remember/client/internal/naming"
	"github.com/faulander/remember/client/internal/reconcile"
	"github.com/faulander/remember/client/internal/remotehttp"
	"github.com/faulander/remember/client/internal/sessionstore"
	"github.com/google/uuid"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const stateEvent = "remember:state"

// RootSelection describes a directory chosen through the native dialog.
type RootSelection struct {
	Path        string `json:"path"`
	Initialized bool   `json:"initialized"`
	Cancelled   bool   `json:"cancelled"`
}

// ObjectView is the UI-safe representation of indexed metadata.
type ObjectView struct {
	ID            string   `json:"id"`
	Type          string   `json:"type"`
	RelativePath  string   `json:"relativePath"`
	IdentityState string   `json:"identityState"`
	Tags          []string `json:"tags"`
	TagKeys       []string `json:"tagKeys"`
}

// IssueView is a local issue. Paths are shown locally and never telemetered.
type IssueView struct {
	Code         string `json:"code"`
	RelativePath string `json:"relativePath"`
	Detail       string `json:"detail"`
}

// NoteView contains only editable note data and its optimistic revision.
type NoteView struct {
	ID           string   `json:"id"`
	RelativePath string   `json:"relativePath"`
	Body         string   `json:"body"`
	Tags         []string `json:"tags"`
	Revision     string   `json:"revision"`
}

// NoteMutation returns the edited note and immediately reconciled tree.
type NoteMutation struct {
	Note  NoteView    `json:"note"`
	State ClientState `json:"state"`
}

// SaveNoteRequest is an optimistic editable-note update.
type SaveNoteRequest struct {
	RelativePath     string   `json:"relativePath"`
	ExpectedRevision string   `json:"expectedRevision"`
	Body             string   `json:"body"`
	Tags             []string `json:"tags"`
}

// MoveNoteRequest renames or moves one note without overwriting.
type MoveNoteRequest struct {
	RelativePath     string `json:"relativePath"`
	ExpectedRevision string `json:"expectedRevision"`
	Folder           string `json:"folder"`
	Name             string `json:"name"`
}

// DeleteNoteRequest moves one note into the local recovery trash.
type DeleteNoteRequest struct {
	RelativePath     string `json:"relativePath"`
	ExpectedRevision string `json:"expectedRevision"`
}

// LoginRequest starts one authenticated desktop session.
type LoginRequest struct {
	ServerURL  string `json:"serverUrl"`
	Email      string `json:"email"`
	Password   string `json:"password"`
	DeviceName string `json:"deviceName"`
}

// SessionView contains public identifiers only; credentials never cross the bridge.
type SessionView struct {
	ServerURL string `json:"serverUrl"`
	UserID    string `json:"userId"`
	DeviceID  string `json:"deviceId"`
	SessionID string `json:"sessionId"`
}

// ManagedSessionView describes one server session without exposing credentials.
type ManagedSessionView struct {
	SessionID  string  `json:"sessionId"`
	DeviceID   string  `json:"deviceId"`
	DeviceName string  `json:"deviceName"`
	Status     string  `json:"status"`
	CreatedAt  string  `json:"createdAt"`
	ExpiresAt  string  `json:"expiresAt"`
	RevokedAt  *string `json:"revokedAt"`
	Current    bool    `json:"current"`
}

// ClientState is the complete render state sent to Svelte.
type ClientState struct {
	Generation      uint64       `json:"generation"`
	Revision        uint64       `json:"revision"`
	Root            string       `json:"root"`
	Objects         []ObjectView `json:"objects"`
	Issues          []IssueView  `json:"issues"`
	AssignedNoteIDs int          `json:"assignedNoteIds"`
}

// DesktopApp adapts the headless local core to Wails.
type DesktopApp struct {
	mu sync.Mutex

	ctx          context.Context
	cancel       context.CancelFunc
	shuttingDown bool
	operations   sync.WaitGroup
	rootOps      sync.Mutex
	authOps      sync.Mutex
	stateMu      sync.Mutex

	core        *coreapp.LocalCore
	watchStop   context.CancelFunc
	forwardDone chan struct{}
	generation  uint64
	revision    uint64
	emit        func(context.Context, string, ...interface{})
	session     *remotehttp.Session
	remote      *remotehttp.Client
	serverURL   string
	credentials sessionstore.Store
}

func NewDesktopApp() *DesktopApp {
	return &DesktopApp{emit: runtime.EventsEmit, credentials: sessionstore.NewKeyringStore()}
}

func (a *DesktopApp) startup(ctx context.Context) {
	a.mu.Lock()
	a.ctx, a.cancel = context.WithCancel(ctx)
	a.mu.Unlock()
}

func (a *DesktopApp) shutdown(context.Context) {
	a.mu.Lock()
	a.shuttingDown = true
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()

	a.operations.Wait()
	a.rootOps.Lock()
	_, _ = a.detachCoreLocked()
	a.rootOps.Unlock()
	a.mu.Lock()
	a.session, a.remote, a.serverURL = nil, nil, ""
	a.mu.Unlock()
}

// SelectRoot opens a native directory chooser and performs no filesystem
// mutation beyond inspecting whether Remember metadata exists.
func (a *DesktopApp) SelectRoot() (RootSelection, error) {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return RootSelection{}, err
	}
	defer done()

	selected, err := runtime.OpenDirectoryDialog(ctx, runtime.OpenDialogOptions{
		Title: "Notizordner auswählen",
	})
	if err != nil {
		return RootSelection{}, fmt.Errorf("select note root: %w", err)
	}
	if selected == "" {
		return RootSelection{Cancelled: true}, nil
	}
	absolute, err := filepath.Abs(selected)
	if err != nil {
		return RootSelection{}, fmt.Errorf("resolve selected root: %w", err)
	}
	_, err = os.Lstat(filepath.Join(absolute, ".remember"))
	return RootSelection{Path: absolute, Initialized: err == nil}, nil
}

// InitializeRoot adopts an unmanaged directory.
func (a *DesktopApp) InitializeRoot(path string) (ClientState, error) {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return ClientState{}, err
	}
	defer done()
	a.rootOps.Lock()
	defer a.rootOps.Unlock()

	core, report, err := coreapp.Initialize(ctx, path)
	if err != nil {
		return ClientState{}, err
	}
	if err := ctx.Err(); err != nil {
		core.Close()
		return ClientState{}, err
	}
	return a.attachCoreLocked(ctx, core, report)
}

// OpenRoot opens an existing Remember directory, including degraded index
// recovery mode.
func (a *DesktopApp) OpenRoot(path string) (ClientState, error) {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return ClientState{}, err
	}
	defer done()
	a.rootOps.Lock()
	defer a.rootOps.Unlock()

	a.mu.Lock()
	current := a.core
	a.mu.Unlock()
	if current != nil {
		absolute, absErr := filepath.Abs(path)
		if absErr == nil && current.Root() == absolute {
			return a.refreshLocked(ctx, current)
		}
	}

	core, report, err := coreapp.Open(ctx, path)
	if err != nil {
		return ClientState{}, err
	}
	if err := ctx.Err(); err != nil {
		core.Close()
		return ClientState{}, err
	}
	return a.attachCoreLocked(ctx, core, report)
}

// Refresh performs a full reconciliation regardless of watcher history.
func (a *DesktopApp) Refresh() (ClientState, error) {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return ClientState{}, err
	}
	defer done()
	a.rootOps.Lock()
	defer a.rootOps.Unlock()

	a.mu.Lock()
	core := a.core
	a.mu.Unlock()
	if core == nil {
		return ClientState{}, coreapp.ErrNotInitialized
	}
	return a.refreshLocked(ctx, core)
}

// ReadNote loads one editable note without exposing it in global events.
func (a *DesktopApp) ReadNote(relativePath string) (NoteView, error) {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return NoteView{}, err
	}
	defer done()
	a.rootOps.Lock()
	defer a.rootOps.Unlock()
	core, err := a.currentCore()
	if err != nil {
		return NoteView{}, err
	}
	document, err := core.ReadNote(ctx, relativePath)
	if err != nil {
		return NoteView{}, err
	}
	return noteView(document), nil
}

// CreateNote creates a Markdown note in an existing folder. An empty name is
// assigned the first available localized default without overwriting files.
func (a *DesktopApp) CreateNote(folder, name string) (NoteMutation, error) {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return NoteMutation{}, err
	}
	defer done()
	a.rootOps.Lock()
	defer a.rootOps.Unlock()
	core, err := a.currentCore()
	if err != nil {
		return NoteMutation{}, err
	}
	blankName := strings.TrimSpace(name) == ""
	for sequence := 1; sequence <= 10_000; sequence++ {
		candidate := name
		if blankName {
			candidate = "Neue Notiz"
			if sequence > 1 {
				candidate = fmt.Sprintf("Neue Notiz %d", sequence)
			}
		}
		relative, title, pathErr := editableNotePath(folder, candidate)
		if pathErr != nil {
			return NoteMutation{}, pathErr
		}
		document, report, createErr := core.CreateNote(ctx, relative, "# "+title+"\n", nil)
		if errors.Is(createErr, coreapp.ErrDestinationUsed) && blankName {
			continue
		}
		if createErr != nil {
			return NoteMutation{}, createErr
		}
		state, stateErr := a.snapshotCurrentState(ctx, core, report)
		if stateErr != nil {
			return NoteMutation{}, stateErr
		}
		return NoteMutation{Note: noteView(document), State: state}, nil
	}
	return NoteMutation{}, errors.New("no available default note name")
}

// CreateFolder creates an empty folder below an existing folder.
func (a *DesktopApp) CreateFolder(parent, name string) (ClientState, error) {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return ClientState{}, err
	}
	defer done()
	a.rootOps.Lock()
	defer a.rootOps.Unlock()
	core, err := a.currentCore()
	if err != nil {
		return ClientState{}, err
	}
	relative, err := editableFolderPath(parent, name)
	if err != nil {
		return ClientState{}, err
	}
	report, err := core.CreateFolder(ctx, relative)
	if err != nil {
		return ClientState{}, err
	}
	return a.snapshotCurrentState(ctx, core, report)
}

// SaveNote updates editable fields if the on-disk revision is unchanged.
func (a *DesktopApp) SaveNote(request SaveNoteRequest) (NoteMutation, error) {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return NoteMutation{}, err
	}
	defer done()
	a.rootOps.Lock()
	defer a.rootOps.Unlock()
	core, err := a.currentCore()
	if err != nil {
		return NoteMutation{}, err
	}
	document, report, err := core.SaveNote(ctx, request.RelativePath, request.ExpectedRevision, request.Body, request.Tags)
	if err != nil {
		return NoteMutation{}, err
	}
	state, err := a.snapshotCurrentState(ctx, core, report)
	if err != nil {
		return NoteMutation{}, err
	}
	return NoteMutation{Note: noteView(document), State: state}, nil
}

// MoveNote renames or moves a note among existing real folders.
func (a *DesktopApp) MoveNote(request MoveNoteRequest) (NoteMutation, error) {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return NoteMutation{}, err
	}
	defer done()
	a.rootOps.Lock()
	defer a.rootOps.Unlock()
	core, err := a.currentCore()
	if err != nil {
		return NoteMutation{}, err
	}
	destination, _, err := editableNotePath(request.Folder, request.Name)
	if err != nil {
		return NoteMutation{}, err
	}
	document, report, err := core.MoveNote(ctx, request.RelativePath, destination, request.ExpectedRevision)
	if err != nil {
		return NoteMutation{}, err
	}
	state, err := a.snapshotCurrentState(ctx, core, report)
	if err != nil {
		return NoteMutation{}, err
	}
	return NoteMutation{Note: noteView(document), State: state}, nil
}

// DeleteNote moves a note into .remember/trash and reconciles immediately.
func (a *DesktopApp) DeleteNote(request DeleteNoteRequest) (ClientState, error) {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return ClientState{}, err
	}
	defer done()
	a.rootOps.Lock()
	defer a.rootOps.Unlock()
	core, err := a.currentCore()
	if err != nil {
		return ClientState{}, err
	}
	report, err := core.DeleteNote(ctx, request.RelativePath, request.ExpectedRevision)
	if err != nil {
		return ClientState{}, err
	}
	return a.snapshotCurrentState(ctx, core, report)
}

// Login authenticates and durably binds the rotating refresh token to the OS keyring.
func (a *DesktopApp) Login(request LoginRequest) (SessionView, error) {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return SessionView{}, err
	}
	defer done()
	a.authOps.Lock()
	defer a.authOps.Unlock()

	a.mu.Lock()
	active := a.session != nil
	a.mu.Unlock()
	if active {
		return SessionView{}, errors.New("already signed in")
	}
	serverURL := strings.TrimSuffix(strings.TrimSpace(request.ServerURL), "/")
	session, err := remotehttp.Login(ctx, serverURL, nil, request.Email, request.Password, request.DeviceName)
	if err != nil {
		return SessionView{}, err
	}
	remote, err := remotehttp.New(serverURL, nil, session)
	if err != nil {
		_ = session.Logout(ctx)
		return SessionView{}, err
	}
	principal := session.Principal()
	if err := session.BindRefreshTokenSink(ctx, a.refreshSink(serverURL, principal)); err != nil {
		_ = session.Logout(ctx)
		return SessionView{}, err
	}
	a.mu.Lock()
	a.session, a.remote, a.serverURL = session, remote, serverURL
	a.mu.Unlock()
	return sessionView(serverURL, principal), nil
}

// RestoreSession rotates the refresh token loaded from the OS keyring before
// exposing a restored session to the desktop.
func (a *DesktopApp) RestoreSession() (*SessionView, error) {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return nil, err
	}
	defer done()
	a.authOps.Lock()
	defer a.authOps.Unlock()

	a.mu.Lock()
	active, serverURL, activePrincipal := a.session, a.serverURL, remotehttp.Principal{}
	if active != nil {
		activePrincipal = active.Principal()
	}
	a.mu.Unlock()
	if active != nil {
		view := sessionView(serverURL, activePrincipal)
		return &view, nil
	}
	credential, err := a.credentials.Load()
	if errors.Is(err, sessionstore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	principal, err := storedPrincipal(credential)
	if err != nil {
		_ = a.credentials.Delete()
		return nil, err
	}
	session, err := remotehttp.Resume(ctx, credential.ServerURL, nil, principal, credential.RefreshToken)
	if errors.Is(err, remotehttp.ErrReauthRequired) {
		if deleteErr := a.credentials.Delete(); deleteErr != nil {
			return nil, deleteErr
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	remote, err := remotehttp.New(credential.ServerURL, nil, session)
	if err != nil {
		_ = session.Logout(ctx)
		return nil, err
	}
	if err := session.BindRefreshTokenSink(ctx, a.refreshSink(credential.ServerURL, principal)); err != nil {
		_ = session.Logout(ctx)
		_ = a.credentials.Delete()
		return nil, err
	}
	a.mu.Lock()
	a.session, a.remote, a.serverURL = session, remote, credential.ServerURL
	a.mu.Unlock()
	view := sessionView(credential.ServerURL, principal)
	return &view, nil
}

func (a *DesktopApp) refreshSink(serverURL string, principal remotehttp.Principal) remotehttp.RefreshTokenSink {
	credential := sessionstore.Credential{
		ServerURL: serverURL, UserID: principal.UserID.String(),
		DeviceID: principal.DeviceID.String(), SessionID: principal.SessionID.String(),
	}
	return remotehttp.RefreshTokenSinkFunc(func(_ context.Context, token string, expiresAt time.Time) error {
		credential.RefreshToken = token
		credential.RefreshExpiresAt = expiresAt
		return a.credentials.Save(credential)
	})
}

func storedPrincipal(credential sessionstore.Credential) (remotehttp.Principal, error) {
	user, userErr := uuid.Parse(credential.UserID)
	device, deviceErr := uuid.Parse(credential.DeviceID)
	session, sessionErr := uuid.Parse(credential.SessionID)
	if userErr != nil || deviceErr != nil || sessionErr != nil {
		return remotehttp.Principal{}, errors.New("stored session is invalid")
	}
	return remotehttp.Principal{UserID: user, DeviceID: device, SessionID: session}, nil
}

func sessionView(serverURL string, principal remotehttp.Principal) SessionView {
	return SessionView{
		ServerURL: serverURL,
		UserID:    principal.UserID.String(), DeviceID: principal.DeviceID.String(), SessionID: principal.SessionID.String(),
	}
}

// ListSessions returns every session belonging to the authenticated account.
func (a *DesktopApp) ListSessions() ([]ManagedSessionView, error) {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return nil, err
	}
	defer done()
	a.authOps.Lock()
	defer a.authOps.Unlock()
	a.mu.Lock()
	session := a.session
	a.mu.Unlock()
	if session == nil {
		return nil, remotehttp.ErrReauthRequired
	}
	items, err := session.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]ManagedSessionView, 0, len(items))
	for _, item := range items {
		view := ManagedSessionView{
			SessionID: item.SessionID.String(), DeviceID: item.DeviceID.String(),
			DeviceName: item.DeviceName, Status: item.Status, CreatedAt: item.CreatedAt.UTC().Format(time.RFC3339Nano),
			ExpiresAt: item.ExpiresAt.UTC().Format(time.RFC3339Nano), Current: item.Current,
		}
		if item.RevokedAt != nil {
			value := item.RevokedAt.UTC().Format(time.RFC3339Nano)
			view.RevokedAt = &value
		}
		result = append(result, view)
	}
	return result, nil
}

// RenameCurrentDevice updates the server-visible name of this device.
func (a *DesktopApp) RenameCurrentDevice(name string) error {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return err
	}
	defer done()
	a.authOps.Lock()
	defer a.authOps.Unlock()
	a.mu.Lock()
	session := a.session
	a.mu.Unlock()
	if session == nil {
		return remotehttp.ErrReauthRequired
	}
	return session.RenameDevice(ctx, session.Principal().DeviceID, name)
}

// RevokeSession revokes another account session; the current session uses Logout.
func (a *DesktopApp) RevokeSession(rawID string) error {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return err
	}
	defer done()
	a.authOps.Lock()
	defer a.authOps.Unlock()
	id, err := uuid.Parse(rawID)
	if err != nil {
		return errors.New("invalid session id")
	}
	a.mu.Lock()
	session := a.session
	a.mu.Unlock()
	if session == nil {
		return remotehttp.ErrReauthRequired
	}
	if id == session.Principal().SessionID {
		return errors.New("use logout for current session")
	}
	return session.RevokeSession(ctx, id)
}

// RevokeDevice revokes another device and all of its account sessions.
func (a *DesktopApp) RevokeDevice(rawID string) error {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return err
	}
	defer done()
	a.authOps.Lock()
	defer a.authOps.Unlock()
	id, err := uuid.Parse(rawID)
	if err != nil {
		return errors.New("invalid device id")
	}
	a.mu.Lock()
	session := a.session
	a.mu.Unlock()
	if session == nil {
		return remotehttp.ErrReauthRequired
	}
	if id == session.Principal().DeviceID {
		return errors.New("use logout for current device")
	}
	return session.RevokeDevice(ctx, id)
}

// SyncNow runs one bounded authenticated foreground synchronization cycle.
func (a *DesktopApp) SyncNow() (ClientState, error) {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return ClientState{}, err
	}
	defer done()
	a.rootOps.Lock()
	defer a.rootOps.Unlock()
	a.authOps.Lock()
	defer a.authOps.Unlock()

	core, err := a.currentCore()
	if err != nil {
		return ClientState{}, err
	}
	a.mu.Lock()
	remote := a.remote
	a.mu.Unlock()
	if remote == nil {
		return ClientState{}, remotehttp.ErrReauthRequired
	}
	if err := core.SyncOnce(ctx, remote); err != nil {
		return ClientState{}, err
	}
	return a.refreshLocked(ctx, core)
}

// Logout revokes the active remote session before removing local credentials.
func (a *DesktopApp) Logout() error {
	ctx, done, err := a.beginOperation()
	if err != nil {
		return err
	}
	defer done()
	a.authOps.Lock()
	defer a.authOps.Unlock()

	a.mu.Lock()
	session := a.session
	a.mu.Unlock()
	if session == nil {
		return nil
	}
	if err := session.Logout(ctx); err != nil {
		return err
	}
	a.mu.Lock()
	if a.session == session {
		a.session, a.remote, a.serverURL = nil, nil, ""
	}
	a.mu.Unlock()
	_ = a.credentials.Delete()
	return nil
}

// NormalizeTags applies the canonical Go Unicode-folding tag policy for UI edits.
func (a *DesktopApp) NormalizeTags(tags []string) ([]string, error) {
	return frontmatter.NormalizeTags(tags)
}

// CloseRoot releases the current root lock and returns the new UI generation.
func (a *DesktopApp) CloseRoot() (uint64, error) {
	_, done, err := a.beginOperation()
	if err != nil {
		return 0, err
	}
	defer done()
	a.rootOps.Lock()
	defer a.rootOps.Unlock()
	return a.detachCoreLocked()
}

func (a *DesktopApp) beginOperation() (context.Context, func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.shuttingDown || a.ctx == nil {
		return nil, nil, errors.New("application is shutting down")
	}
	a.operations.Add(1)
	return a.ctx, a.operations.Done, nil
}

func (a *DesktopApp) refreshLocked(ctx context.Context, core *coreapp.LocalCore) (ClientState, error) {
	report, err := core.Reconcile(ctx)
	if err != nil {
		return ClientState{}, err
	}
	return a.snapshotCurrentState(ctx, core, report)
}

// attachCoreLocked starts the replacement watcher before publishing the new
// root. The caller holds rootOps.
func (a *DesktopApp) attachCoreLocked(ctx context.Context, core *coreapp.LocalCore, report reconcile.Report) (ClientState, error) {
	a.stateMu.Lock()
	state, err := stateFromCore(ctx, core, report, 0, 0)
	if err != nil {
		a.stateMu.Unlock()
		core.Close()
		return ClientState{}, err
	}
	watchCtx, stop := context.WithCancel(ctx)
	updates, err := core.StartWatching(watchCtx)
	if err != nil {
		a.stateMu.Unlock()
		stop()
		core.Close()
		return ClientState{}, err
	}

	a.mu.Lock()
	oldCore := a.core
	oldStop := a.watchStop
	oldDone := a.forwardDone
	a.generation++
	a.revision = 0
	generation := a.generation
	done := make(chan struct{})
	a.core = core
	a.watchStop = stop
	a.forwardDone = done
	wailsCtx := a.ctx
	a.mu.Unlock()
	state.Generation = generation
	state.Revision = 0
	a.stateMu.Unlock()

	go a.forwardUpdates(wailsCtx, core, generation, updates, done)

	if oldStop != nil {
		oldStop()
	}
	if oldCore != nil {
		_ = oldCore.Close()
	}
	if oldDone != nil {
		<-oldDone
	}
	return state, nil
}

func (a *DesktopApp) forwardUpdates(ctx context.Context, core *coreapp.LocalCore, generation uint64, updates <-chan coreapp.Update, done chan<- struct{}) {
	defer close(done)
	for update := range updates {
		if update.Err != nil {
			revision, current := a.currentRevisionIfCurrent(core, generation)
			if !current {
				return
			}
			a.emit(ctx, stateEvent, map[string]any{
				"generation": generation, "revision": revision, "error": update.Err.Error(),
			})
			continue
		}
		state, err := a.snapshotCurrentState(ctx, core, update.Report)
		if err != nil {
			revision, current := a.currentRevisionIfCurrent(core, generation)
			if !current {
				return
			}
			a.emit(ctx, stateEvent, map[string]any{
				"generation": generation, "revision": revision, "error": err.Error(),
			})
			continue
		}
		if state.Generation != generation {
			return
		}
		a.emit(ctx, stateEvent, map[string]any{
			"generation": generation, "revision": state.Revision, "state": state,
		})
	}
}

func (a *DesktopApp) snapshotCurrentState(ctx context.Context, core *coreapp.LocalCore, report reconcile.Report) (ClientState, error) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	state, err := stateFromCore(ctx, core, report, 0, 0)
	if err != nil {
		return ClientState{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.core != core || a.shuttingDown {
		return ClientState{}, coreapp.ErrCoreClosed
	}
	a.revision++
	state.Generation = a.generation
	state.Revision = a.revision
	return state, nil
}

func (a *DesktopApp) currentRevisionIfCurrent(core *coreapp.LocalCore, generation uint64) (uint64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.generation != generation || a.core != core || a.shuttingDown {
		return 0, false
	}
	return a.revision, true
}

// detachCoreLocked is called while rootOps is held.
func (a *DesktopApp) detachCoreLocked() (uint64, error) {
	a.mu.Lock()
	core := a.core
	stop := a.watchStop
	done := a.forwardDone
	a.generation++
	a.revision = 0
	generation := a.generation
	a.core = nil
	a.watchStop = nil
	a.forwardDone = nil
	a.mu.Unlock()

	if stop != nil {
		stop()
	}
	var closeErr error
	if core != nil {
		closeErr = core.Close()
	}
	if done != nil {
		<-done
	}
	return generation, closeErr
}

func stateFromCore(ctx context.Context, core *coreapp.LocalCore, report reconcile.Report, generation, revision uint64) (ClientState, error) {
	snapshot, err := core.Snapshot(ctx)
	if err != nil {
		return ClientState{}, err
	}
	state := ClientState{
		Generation: generation, Revision: revision, Root: core.Root(), AssignedNoteIDs: report.AssignedNoteIDs,
		Objects: make([]ObjectView, 0, len(snapshot.Objects)),
		Issues:  make([]IssueView, 0, len(snapshot.Issues)),
	}
	objectPaths := make(map[string]string, len(snapshot.Objects))
	for _, object := range snapshot.Objects {
		objectPaths[object.ID.String()] = object.RelativePath
		view := objectView(object)
		if object.Type == localindex.ObjectNote {
			if document, readErr := core.ReadNote(ctx, object.RelativePath); readErr == nil {
				view.Tags = document.Tags
				view.TagKeys = make([]string, len(document.Tags))
				for i, tag := range document.Tags {
					view.TagKeys[i] = naming.CollisionKey(tag)
				}
			}
		}
		state.Objects = append(state.Objects, view)
	}
	for _, issue := range snapshot.Issues {
		state.Issues = append(state.Issues, IssueView{Code: issue.Code, RelativePath: issue.RelativePath, Detail: issue.Detail})
	}
	incidents, err := core.IntegrityIncidents(ctx, 100)
	if err != nil {
		return ClientState{}, err
	}
	for _, incident := range incidents {
		code := "sync_" + incident.Code
		detail := "Synchronisierter Inhalt fehlt oder stimmt nicht mit seinem Hash überein."
		state.Issues = append(state.Issues, IssueView{Code: code, RelativePath: objectPaths[incident.ObjectID.String()], Detail: detail})
	}
	sort.Slice(state.Objects, func(i, j int) bool { return state.Objects[i].RelativePath < state.Objects[j].RelativePath })
	sort.Slice(state.Issues, func(i, j int) bool {
		if state.Issues[i].RelativePath == state.Issues[j].RelativePath {
			return state.Issues[i].Code < state.Issues[j].Code
		}
		return state.Issues[i].RelativePath < state.Issues[j].RelativePath
	})
	return state, nil
}

func (a *DesktopApp) currentCore() (*coreapp.LocalCore, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.core == nil {
		return nil, coreapp.ErrNotInitialized
	}
	return a.core, nil
}

func editableFolderPath(parent, input string) (string, error) {
	name, err := naming.NormalizeAndValidateComponent(strings.TrimSpace(input))
	if err != nil {
		return "", err
	}
	if parent == "" {
		return name, nil
	}
	if err := naming.ValidateUserRelativePath(parent); err != nil {
		return "", err
	}
	return path.Join(parent, name), nil
}

func editableNotePath(folder, input string) (string, string, error) {
	name := strings.TrimSpace(input)
	if strings.EqualFold(filepath.Ext(name), ".md") {
		name = name[:len(name)-len(filepath.Ext(name))]
	}
	name, err := naming.NormalizeAndValidateComponent(name)
	if err != nil {
		return "", "", err
	}
	fileName := name + ".md"
	if err := naming.ValidateComponent(fileName); err != nil {
		return "", "", err
	}
	if folder == "" {
		return fileName, name, nil
	}
	if err := naming.ValidateUserRelativePath(folder); err != nil {
		return "", "", err
	}
	return path.Join(folder, fileName), name, nil
}

func noteView(document coreapp.NoteDocument) NoteView {
	return NoteView{
		ID: document.ID.String(), RelativePath: document.RelativePath,
		Body: document.Body, Tags: document.Tags, Revision: document.Revision,
	}
}

func objectView(object localindex.Object) ObjectView {
	return ObjectView{
		ID: object.ID.String(), Type: string(object.Type),
		RelativePath: object.RelativePath, IdentityState: string(object.IdentityState),
	}
}
