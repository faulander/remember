package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	clientapp "github.com/faulander/remember/client/internal/app"
	"github.com/faulander/remember/client/internal/clientsync"
	"github.com/faulander/remember/client/internal/frontmatter"
	"github.com/faulander/remember/client/internal/localindex"
	"github.com/faulander/remember/client/internal/reconcile"
	"github.com/faulander/remember/client/internal/remotehttp"
	"github.com/faulander/remember/server/integrationtest"
	"github.com/google/uuid"
)

func login(t *testing.T, base, email, password, device string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": password, "device_name": device})
	response, err := http.Post(base+"/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login %s status=%d", device, response.StatusCode)
	}
	var result struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil || result.Tokens.AccessToken == "" {
		t.Fatalf("login %s token error=%v", device, err)
	}
	return result.Tokens.AccessToken
}

func remote(t *testing.T, base, token string) *remotehttp.Client {
	t.Helper()
	client, err := remotehttp.New(base, nil, remotehttp.AccessTokenSourceFunc(func(context.Context) (string, error) { return token, nil }))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type interruptingRemote struct {
	*remotehttp.Client
	pulls     int
	afters    []uint64
	firstNext uint64
}

func (r *interruptingRemote) PreserveAndDeleteEmptyFolder(ctx context.Context, a, b, c uuid.UUID, d uint64) (remotehttp.PreserveDeleteFolderResult, error) {
	return r.Client.PreserveAndDeleteEmptyFolder(ctx, a, b, c, d)
}
func (r *interruptingRemote) Pull(ctx context.Context, after uint64, limit int) (remotehttp.PullPage, error) {
	r.afters = append(r.afters, after)
	if r.pulls == 1 {
		return remotehttp.PullPage{}, errors.New("simulated pull-page interruption")
	}
	r.pulls++
	page, err := r.Client.Pull(ctx, after, limit)
	if err == nil && r.firstNext == 0 {
		r.firstNext = page.NextCursor
	}
	return page, err
}

type recordingRemote struct {
	*remotehttp.Client
	afters []uint64
}

func (r *recordingRemote) PreserveAndDeleteEmptyFolder(ctx context.Context, a, b, c uuid.UUID, d uint64) (remotehttp.PreserveDeleteFolderResult, error) {
	return r.Client.PreserveAndDeleteEmptyFolder(ctx, a, b, c, d)
}
func (r *recordingRemote) Pull(ctx context.Context, after uint64, limit int) (remotehttp.PullPage, error) {
	r.afters = append(r.afters, after)
	return r.Client.Pull(ctx, after, limit)
}

func syncTimes(t *testing.T, ctx context.Context, core *clientapp.LocalCore, remote *remotehttp.Client, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		if err := core.SyncOnce(ctx, remote); err != nil {
			t.Fatalf("sync %d/%d: %v", i+1, count, err)
		}
	}
}

func TestAuthenticatedBlobIntegrityAlarmsResumeAfterRestart(t *testing.T) {
	ctx := context.Background()
	server, err := integrationtest.New(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	const email, password = "integrity@example.test", "correct horse battery staple"
	if err := server.CreateVerifiedUser(ctx, email, password); err != nil {
		t.Fatal(err)
	}
	remoteA := remote(t, server.URL, login(t, server.URL, email, password, "Integrity A"))
	rootA := t.TempDir()
	a, _, err := clientapp.Initialize(ctx, rootA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	note, _, err := a.CreateNote(ctx, "Alarm.md", "exact alarm bytes\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	expected, err := os.ReadFile(filepath.Join(rootA, "Alarm.md"))
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse(server.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var mode atomic.Int32
	fault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/blobs/") {
			switch mode.Load() {
			case 0:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"blob_not_found"}`))
				return
			case 1:
				wrong := []byte("wrong authenticated blob")
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", fmt.Sprint(len(wrong)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(wrong)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	defer fault.Close()
	remoteB := remote(t, fault.URL, login(t, server.URL, email, password, "Integrity B"))
	rootB := t.TempDir()
	b, _, err := clientapp.Initialize(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remoteB); !errors.Is(err, clientsync.ErrBlobMissing) {
		t.Fatalf("missing sync=%v", err)
	}
	incidents, err := b.IntegrityIncidents(ctx, 10)
	if err != nil || len(incidents) != 1 || incidents[0].Code != "missing_blob" || incidents[0].ObjectID != note.ID || incidents[0].OccurrenceCount != 1 {
		t.Fatalf("missing incidents=%#v err=%v", incidents, err)
	}
	index, err := localindex.Open(ctx, filepath.Join(rootB, ".remember", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, _ := clientsync.NewStore(index)
	confirmed, _ := store.ConfirmedCursor(ctx)
	downloaded, _ := store.DownloadedCursor(ctx)
	active, _ := store.ActiveApplyPlan(ctx)
	index.Close()
	if confirmed >= downloaded || active == nil {
		t.Fatalf("frontiers confirmed/downloaded=%d/%d active=%#v", confirmed, downloaded, active)
	}
	planID := active.ID
	if _, err := os.Stat(filepath.Join(rootB, "Alarm.md")); !os.IsNotExist(err) {
		t.Fatalf("alarm note published: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b, _, err = clientapp.Open(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.SyncOnce(ctx, remoteB); !errors.Is(err, clientsync.ErrBlobMissing) {
		t.Fatalf("restart missing sync=%v", err)
	}
	incidents, err = b.IntegrityIncidents(ctx, 10)
	if err != nil || len(incidents) != 1 || incidents[0].OccurrenceCount != 2 {
		t.Fatalf("deduplicated incidents=%#v err=%v", incidents, err)
	}
	assertBlocked := func(stage string) {
		t.Helper()
		index, err := localindex.Open(ctx, filepath.Join(rootB, ".remember", "index.db"))
		if err != nil {
			t.Fatal(err)
		}
		store, err := clientsync.NewStore(index)
		if err != nil {
			index.Close()
			t.Fatal(err)
		}
		confirmed, err := store.ConfirmedCursor(ctx)
		if err != nil {
			index.Close()
			t.Fatal(err)
		}
		downloaded, err := store.DownloadedCursor(ctx)
		if err != nil {
			index.Close()
			t.Fatal(err)
		}
		active, err := store.ActiveApplyPlan(ctx)
		if err != nil {
			index.Close()
			t.Fatal(err)
		}
		if err := index.Close(); err != nil {
			t.Fatal(err)
		}
		if confirmed >= downloaded || active == nil || active.ID != planID {
			t.Fatalf("%s frontiers=%d/%d active=%#v want plan=%s", stage, confirmed, downloaded, active, planID)
		}
		if _, err := os.Stat(filepath.Join(rootB, "Alarm.md")); !os.IsNotExist(err) {
			t.Fatalf("%s alarm note published: %v", stage, err)
		}
	}
	assertBlocked("restart missing")
	mode.Store(1)
	if err := b.SyncOnce(ctx, remoteB); !errors.Is(err, clientsync.ErrBlobHashMismatch) {
		t.Fatalf("corrupt sync=%v", err)
	}
	incidents, _ = b.IntegrityIncidents(ctx, 10)
	codes := map[string]clientsync.IntegrityIncident{}
	for _, incident := range incidents {
		codes[incident.Code] = incident
	}
	if len(codes) != 2 || codes["hash_mismatch"].ObjectID != note.ID {
		t.Fatalf("corrupt incidents=%#v", incidents)
	}
	assertBlocked("hash mismatch")
	mode.Store(2)
	if err := b.SyncOnce(ctx, remoteB); err != nil {
		t.Fatal(err)
	}
	assertRememberNoteBytesAndID(t, ctx, b, "Alarm.md", expected, note.ID)
	index, err = localindex.Open(ctx, filepath.Join(rootB, ".remember", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, _ = clientsync.NewStore(index)
	finalConfirmed, _ := store.ConfirmedCursor(ctx)
	finalDownloaded, _ := store.DownloadedCursor(ctx)
	active, _ = store.ActiveApplyPlan(ctx)
	index.Close()
	if finalConfirmed != finalDownloaded || active != nil {
		t.Fatalf("final frontiers=%d/%d active=%#v", finalConfirmed, finalDownloaded, active)
	}
}

func TestAuthenticatedEmptyDivergentFolderMovesConverge(t *testing.T) {
	ctx := context.Background()
	server, err := integrationtest.New(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	const email, password = "divergent-empty@example.test", "correct horse battery staple"
	if err := server.CreateVerifiedUser(ctx, email, password); err != nil {
		t.Fatal(err)
	}
	remoteA := remote(t, server.URL, login(t, server.URL, email, password, "Divergent Empty A"))
	remoteB := remote(t, server.URL, login(t, server.URL, email, password, "Divergent Empty B"))
	rootA, rootB := t.TempDir(), t.TempDir()
	a, _, err := clientapp.Initialize(ctx, rootA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, _, err := clientapp.Initialize(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateFolder(ctx, "F"); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	folderID := localFolderIDAtPath(t, ctx, rootA, "F")
	before, err := os.Stat(filepath.Join(rootB, "F"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootB, "F"), filepath.Join(rootB, "Local")); err != nil {
		t.Fatal(err)
	}
	reconcileFolderMove(t, ctx, rootB, "F")
	index, err := localindex.Open(ctx, filepath.Join(rootB, ".remember", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := clientsync.NewStore(index)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := store.ListReady(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var moveOperation uuid.UUID
	for _, item := range ready {
		if item.Mutation.ObjectID == folderID && item.Mutation.Kind == clientsync.Move {
			moveOperation = item.Mutation.OperationID
		}
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	if moveOperation == uuid.Nil {
		t.Fatal("missing divergent move operation")
	}
	if err := os.Rename(filepath.Join(rootA, "F"), filepath.Join(rootA, "Server")); err != nil {
		t.Fatal(err)
	}
	reconcileFolderMove(t, ctx, rootA, "F")
	syncTimes(t, ctx, a, remoteA, 1)
	for i := 0; i < 4; i++ {
		if err := b.SyncOnce(ctx, remoteB); err != nil {
			t.Fatalf("B recovery sync %d: %v", i, err)
		}
	}
	recoveredName := clientsync.ConflictFolderName("Local", moveOperation)
	recoveredPath := clientsync.ConflictRootName + "/" + clientsync.ConflictRecoveredName + "/" + recoveredName
	if got := localFolderIDAtPath(t, ctx, rootB, "Server"); got != folderID {
		t.Fatalf("B canonical id=%s want=%s", got, folderID)
	}
	recoveredID := localFolderIDAtPath(t, ctx, rootB, recoveredPath)
	if recoveredID == uuid.Nil || recoveredID == folderID {
		t.Fatalf("B recovered id=%s", recoveredID)
	}
	recoveredInfo, err := os.Stat(filepath.Join(rootB, filepath.FromSlash(recoveredPath)))
	if err != nil || !os.SameFile(before, recoveredInfo) {
		t.Fatalf("B recovered inode: %v", err)
	}
	for _, old := range []string{"F", "Local"} {
		if _, err := os.Stat(filepath.Join(rootB, old)); !os.IsNotExist(err) {
			t.Fatalf("B retained %s: %v", old, err)
		}
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b, _, err = clientapp.Open(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.SyncOnce(ctx, remoteB); err != nil {
		t.Fatal(err)
	}
	if got := localFolderIDAtPath(t, ctx, rootB, "Server"); got != folderID {
		t.Fatalf("restart canonical id=%s want=%s", got, folderID)
	}
	if got := localFolderIDAtPath(t, ctx, rootB, recoveredPath); got != recoveredID {
		t.Fatalf("restart recovered id=%s want=%s", got, recoveredID)
	}
	restartedInfo, err := os.Stat(filepath.Join(rootB, filepath.FromSlash(recoveredPath)))
	if err != nil || !os.SameFile(before, restartedInfo) {
		t.Fatalf("restart recovered inode: %v", err)
	}
	for _, old := range []string{"F", "Local"} {
		if _, err := os.Stat(filepath.Join(rootB, old)); !os.IsNotExist(err) {
			t.Fatalf("restart B retained %s: %v", old, err)
		}
	}
	syncTimes(t, ctx, a, remoteA, 1)
	if got := localFolderIDAtPath(t, ctx, rootA, "Server"); got != folderID {
		t.Fatalf("A canonical id=%s", got)
	}
	if got := localFolderIDAtPath(t, ctx, rootA, recoveredPath); got != recoveredID {
		t.Fatalf("A recovered id=%s want=%s", got, recoveredID)
	}
	for _, old := range []string{"F", "Local"} {
		if _, err := os.Stat(filepath.Join(rootA, old)); !os.IsNotExist(err) {
			t.Fatalf("A retained %s: %v", old, err)
		}
	}
	remoteC := remote(t, server.URL, login(t, server.URL, email, password, "Divergent Empty C"))
	rootC := t.TempDir()
	c, _, err := clientapp.Initialize(ctx, rootC)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	syncTimes(t, ctx, c, remoteC, 1)
	if got := localFolderIDAtPath(t, ctx, rootC, "Server"); got != folderID {
		t.Fatalf("C canonical id=%s", got)
	}
	if got := localFolderIDAtPath(t, ctx, rootC, recoveredPath); got != recoveredID {
		t.Fatalf("C recovered id=%s want=%s", got, recoveredID)
	}
	if _, err := os.Stat(filepath.Join(rootC, "F")); !os.IsNotExist(err) {
		t.Fatalf("C retained original path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootC, "Local")); !os.IsNotExist(err) {
		t.Fatalf("C retained losing path: %v", err)
	}
}

func TestAuthenticatedDivergentFolderDirectNotesConverge(t *testing.T) {
	ctx := context.Background()
	server, err := integrationtest.New(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	const email, password = "divergent-notes@example.test", "correct horse battery staple"
	if err := server.CreateVerifiedUser(ctx, email, password); err != nil {
		t.Fatal(err)
	}
	remoteA := remote(t, server.URL, login(t, server.URL, email, password, "Divergent Notes A"))
	remoteB := remote(t, server.URL, login(t, server.URL, email, password, "Divergent Notes B"))
	rootA, rootB := t.TempDir(), t.TempDir()
	a, _, err := clientapp.Initialize(ctx, rootA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, _, err := clientapp.Initialize(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateFolder(ctx, "F"); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	folderID := localFolderIDAtPath(t, ctx, rootB, "F")
	if err := os.Rename(filepath.Join(rootB, "F"), filepath.Join(rootB, "Local")); err != nil {
		t.Fatal(err)
	}
	reconcileFolderMove(t, ctx, rootB, "F")
	note, _, err := b.CreateNote(ctx, "Local/N.md", "first\n", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	current, err := b.ReadNote(ctx, "Local/N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.SaveNote(ctx, "Local/N.md", current.Revision, "final authenticated bytes\n", []string{"final"}); err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile(filepath.Join(rootB, "Local", "N.md"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(rootB, "Local"))
	if err != nil {
		t.Fatal(err)
	}
	index, err := localindex.Open(ctx, filepath.Join(rootB, ".remember", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, _ := clientsync.NewStore(index)
	ready, err := store.ListReady(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var moveOperation uuid.UUID
	for _, item := range ready {
		if item.Mutation.ObjectID == folderID && item.Mutation.Kind == clientsync.Move {
			moveOperation = item.Mutation.OperationID
		}
	}
	index.Close()
	if moveOperation == uuid.Nil {
		t.Fatal("missing move operation")
	}
	if err := os.Rename(filepath.Join(rootA, "F"), filepath.Join(rootA, "Server")); err != nil {
		t.Fatal(err)
	}
	reconcileFolderMove(t, ctx, rootA, "F")
	syncTimes(t, ctx, a, remoteA, 1)
	for i := 0; i < 7; i++ {
		if err := b.SyncOnce(ctx, remoteB); err != nil && !errors.Is(err, clientapp.ErrUnresolvedOutbound) {
			t.Fatalf("B recovery %d: %v", i, err)
		}
	}
	recovered := clientsync.ConflictRootName + "/" + clientsync.ConflictRecoveredName + "/" + clientsync.ConflictFolderName("Local", moveOperation)
	recoveredID := localFolderIDAtPath(t, ctx, rootB, recovered)
	if recoveredID == uuid.Nil || recoveredID == folderID {
		t.Fatalf("recovered id=%s", recoveredID)
	}
	if got := localFolderIDAtPath(t, ctx, rootB, "Server"); got != folderID {
		t.Fatalf("canonical id=%s", got)
	}
	info, err := os.Stat(filepath.Join(rootB, filepath.FromSlash(recovered)))
	if err != nil || !os.SameFile(before, info) {
		t.Fatalf("recovered inode: %v", err)
	}
	assertRememberNoteBytesAndID(t, ctx, b, recovered+"/N.md", expected, note.ID)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b, _, err = clientapp.Open(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.SyncOnce(ctx, remoteB); err != nil {
		t.Fatal(err)
	}
	if got := localFolderIDAtPath(t, ctx, rootB, recovered); got != recoveredID {
		t.Fatalf("restart recovered folder id=%s want=%s", got, recoveredID)
	}
	assertRememberNoteBytesAndID(t, ctx, b, recovered+"/N.md", expected, note.ID)
	restartedInfo, err := os.Stat(filepath.Join(rootB, filepath.FromSlash(recovered)))
	if err != nil || !os.SameFile(before, restartedInfo) {
		t.Fatalf("restart inode: %v", err)
	}
	syncTimes(t, ctx, a, remoteA, 2)
	if got := localFolderIDAtPath(t, ctx, rootA, "Server"); got != folderID {
		t.Fatalf("A canonical=%s", got)
	}
	if got := localFolderIDAtPath(t, ctx, rootA, recovered); got != recoveredID {
		t.Fatalf("A recovered=%s want=%s", got, recoveredID)
	}
	assertRememberNoteBytesAndID(t, ctx, a, recovered+"/N.md", expected, note.ID)
	remoteC := remote(t, server.URL, login(t, server.URL, email, password, "Divergent Notes C"))
	rootC := t.TempDir()
	c, _, err := clientapp.Initialize(ctx, rootC)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	syncTimes(t, ctx, c, remoteC, 1)
	if got := localFolderIDAtPath(t, ctx, rootC, "Server"); got != folderID {
		t.Fatalf("C canonical=%s", got)
	}
	if got := localFolderIDAtPath(t, ctx, rootC, recovered); got != recoveredID {
		t.Fatalf("C recovered=%s want=%s", got, recoveredID)
	}
	assertRememberNoteBytesAndID(t, ctx, c, recovered+"/N.md", expected, note.ID)
	for _, root := range []string{rootA, rootB, rootC} {
		for _, old := range []string{"F", "Local"} {
			if _, err := os.Stat(filepath.Join(root, old)); !os.IsNotExist(err) {
				t.Fatalf("%s retained %s: %v", root, old, err)
			}
		}
	}
}

func TestAuthenticatedRootNoteIsolationBehindDivergentFolderMove(t *testing.T) {
	ctx := context.Background()
	server, err := integrationtest.New(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	const email, password = "isolation@example.test", "correct horse battery staple"
	if err := server.CreateVerifiedUser(ctx, email, password); err != nil {
		t.Fatal(err)
	}
	remoteA := remote(t, server.URL, login(t, server.URL, email, password, "Isolation A"))
	remoteB := remote(t, server.URL, login(t, server.URL, email, password, "Isolation B"))
	rootA, rootB := t.TempDir(), t.TempDir()
	a, _, err := clientapp.Initialize(ctx, rootA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, _, err := clientapp.Initialize(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateFolder(ctx, "FolderX"); err != nil {
		t.Fatal(err)
	}
	blocked, _, err := a.CreateNote(ctx, "FolderX/Blocked.md", "structural blocker\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	y, _, err := a.CreateNote(ctx, "Y.md", "initial Y\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	z, _, err := a.CreateNote(ctx, "Z.md", "initial Z\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	folderID := localFolderIDAtPath(t, ctx, rootA, "FolderX")
	if got := localFolderIDAtPath(t, ctx, rootB, "FolderX"); got != folderID {
		t.Fatalf("folder ids A/B=%s/%s", folderID, got)
	}
	index, err := localindex.Open(ctx, filepath.Join(rootB, ".remember", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := clientsync.NewStore(index)
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.ConfirmedCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	index.Close()
	if err := os.Rename(filepath.Join(rootB, "FolderX"), filepath.Join(rootB, "LocalFolder")); err != nil {
		t.Fatal(err)
	}
	reconcileFolderMove(t, ctx, rootB, "FolderX")
	localBefore, err := os.Stat(filepath.Join(rootB, "LocalFolder"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(rootA, "FolderX"), filepath.Join(rootA, "RemoteFolder")); err != nil {
		t.Fatal(err)
	}
	reconcileFolderMove(t, ctx, rootA, "FolderX")
	yCurrent, err := a.ReadNote(ctx, "Y.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.SaveNote(ctx, "Y.md", yCurrent.Revision, "remote exact Y\n", []string{"adr57"}); err != nil {
		t.Fatal(err)
	}
	zCurrent, err := a.ReadNote(ctx, "Z.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DeleteNote(ctx, "Z.md", zCurrent.Revision); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	expectedBlocked, err := os.ReadFile(filepath.Join(rootA, "RemoteFolder", "Blocked.md"))
	if err != nil {
		t.Fatal(err)
	}
	expectedY, err := os.ReadFile(filepath.Join(rootA, "Y.md"))
	if err != nil {
		t.Fatal(err)
	}
	q, _, err := b.CreateNote(ctx, "Q.md", "independent outbound Q\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	expectedQ, err := os.ReadFile(filepath.Join(rootB, "Q.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SyncOnce(ctx, remoteB); !errors.Is(err, clientapp.ErrUnresolvedOutbound) {
		t.Fatalf("blocked sync=%v", err)
	}
	assertRememberNoteBytesAndID(t, ctx, b, "Y.md", expectedY, y.ID)
	if _, err := b.ReadNote(ctx, "Z.md"); err == nil {
		t.Fatal("B retained deleted Z")
	}
	if info, err := os.Stat(filepath.Join(rootB, "LocalFolder")); err != nil || !os.SameFile(localBefore, info) {
		t.Fatalf("local folder inode changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootB, "RemoteFolder")); !os.IsNotExist(err) {
		t.Fatalf("B materialized losing remote folder: %v", err)
	}
	index, err = localindex.Open(ctx, filepath.Join(rootB, ".remember", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, err = clientsync.NewStore(index)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err := store.DownloadedCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := store.ConfirmedCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed != base || downloaded <= confirmed {
		t.Fatalf("frontiers confirmed/downloaded/base=%d/%d/%d", confirmed, downloaded, base)
	}
	unresolved, err := store.HasUnresolvedLocalIntent(ctx, folderID)
	if err != nil || !unresolved {
		t.Fatalf("folder intent unresolved=%v err=%v", unresolved, err)
	}
	states := map[uuid.UUID]string{}
	for cursor := base + 1; cursor <= downloaded; cursor++ {
		item, found, err := store.InboxChange(ctx, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			states[item.Change.ObjectID] = item.State
		}
	}
	if states[folderID] != "pending" || states[y.ID] != "applied" || states[z.ID] != "applied" {
		t.Fatalf("inbox X/Y/Z=%q/%q/%q", states[folderID], states[y.ID], states[z.ID])
	}
	index.Close()
	yBeforeRestart, err := os.Stat(filepath.Join(rootB, "Y.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b, _, err = clientapp.Open(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	recording := &recordingRemote{Client: remoteB}
	if err := b.SyncOnce(ctx, recording); !errors.Is(err, clientapp.ErrUnresolvedOutbound) {
		t.Fatalf("restart sync=%v", err)
	}
	if len(recording.afters) == 0 || recording.afters[0] != downloaded {
		t.Fatalf("restart pull afters=%v downloaded=%d", recording.afters, downloaded)
	}
	yAfterRestart, err := os.Stat(filepath.Join(rootB, "Y.md"))
	if err != nil || !os.SameFile(yBeforeRestart, yAfterRestart) || !yBeforeRestart.ModTime().Equal(yAfterRestart.ModTime()) {
		t.Fatalf("Y republished on restart: %v", err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	assertRememberNoteBytesAndID(t, ctx, a, "Q.md", expectedQ, q.ID)
	remoteC := remote(t, server.URL, login(t, server.URL, email, password, "Isolation C"))
	rootC := t.TempDir()
	c, _, err := clientapp.Initialize(ctx, rootC)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	syncTimes(t, ctx, c, remoteC, 1)
	if got := localFolderIDAtPath(t, ctx, rootC, "RemoteFolder"); got != folderID {
		t.Fatalf("cold folder id=%s want=%s", got, folderID)
	}
	if _, err := os.Stat(filepath.Join(rootC, "LocalFolder")); !os.IsNotExist(err) {
		t.Fatalf("cold retained local losing path: %v", err)
	}
	assertRememberNoteBytesAndID(t, ctx, c, "RemoteFolder/Blocked.md", expectedBlocked, blocked.ID)
	assertRememberNoteBytesAndID(t, ctx, c, "Y.md", expectedY, y.ID)
	if _, err := c.ReadNote(ctx, "Z.md"); err == nil {
		t.Fatal("cold retained Z")
	}
	assertRememberNoteBytesAndID(t, ctx, c, "Q.md", expectedQ, q.ID)
	if z.ID == uuid.Nil {
		t.Fatal("missing Z id")
	}
}

func assertRememberNoteBytesAndID(t *testing.T, ctx context.Context, core *clientapp.LocalCore, path string, wantBytes []byte, wantID uuid.UUID) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(core.Root(), filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, wantBytes) {
		t.Fatalf("%s bytes differ", path)
	}
	inspection, err := frontmatter.Inspect(got)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.NoteID != wantID {
		t.Fatalf("%s id=%s want=%s", path, inspection.NoteID, wantID)
	}
}

func TestAuthenticatedStructuralConflictsConverge(t *testing.T) {
	ctx := context.Background()
	server, err := integrationtest.New(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	const email, password = "matrix@example.test", "correct horse battery staple"
	if err := server.CreateVerifiedUser(ctx, email, password); err != nil {
		t.Fatal(err)
	}
	remoteA := remote(t, server.URL, login(t, server.URL, email, password, "Matrix A"))
	remoteB := remote(t, server.URL, login(t, server.URL, email, password, "Matrix B"))
	rootA, rootB := t.TempDir(), t.TempDir()
	a, _, err := clientapp.Initialize(ctx, rootA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, _, err := clientapp.Initialize(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	moveDelete, _, err := a.CreateNote(ctx, "MoveDelete.md", "local move must survive delete\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	aNote, err := a.ReadNote(ctx, "MoveDelete.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DeleteNote(ctx, "MoveDelete.md", aNote.Revision); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	bNote, err := b.ReadNote(ctx, "MoveDelete.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.MoveNote(ctx, "MoveDelete.md", "LocallyMoved.md", bNote.Revision); err != nil {
		t.Fatal(err)
	}
	moved, err := b.ReadNote(ctx, "LocallyMoved.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.SaveNote(ctx, "LocallyMoved.md", moved.Revision, "edited local move must survive delete\n", nil); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, b, remoteB, 3)
	syncTimes(t, ctx, a, remoteA, 2)
	for label, core := range map[string]*clientapp.LocalCore{"A": a, "B": b} {
		for _, name := range []string{"MoveDelete.md", "LocallyMoved.md"} {
			if _, err := core.ReadNote(ctx, name); err == nil {
				t.Fatalf("%s retained tombstoned move at %s", label, name)
			}
		}
	}
	assertConflictContent(t, rootA, "edited local move must survive delete")
	assertConflictContent(t, rootB, "edited local move must survive delete")
	deleteMove, _, err := a.CreateNote(ctx, "DeleteMove.md", "remote move must survive local delete\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	bDelete, err := b.ReadNote(ctx, "DeleteMove.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.DeleteNote(ctx, "DeleteMove.md", bDelete.Revision); err != nil {
		t.Fatal(err)
	}
	aMove, err := a.ReadNote(ctx, "DeleteMove.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.MoveNote(ctx, "DeleteMove.md", "RemotelyMoved.md", aMove.Revision); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 3)
	syncTimes(t, ctx, a, remoteA, 2)
	for label, core := range map[string]*clientapp.LocalCore{"A": a, "B": b} {
		for _, name := range []string{"DeleteMove.md", "RemotelyMoved.md"} {
			if _, err := core.ReadNote(ctx, name); err == nil {
				t.Fatalf("%s retained delete/move identity at %s", label, name)
			}
		}
	}
	assertConflictContent(t, rootA, "remote move must survive local delete")
	assertConflictContent(t, rootB, "remote move must survive local delete")
	if _, err := a.CreateFolder(ctx, "Same"); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	if _, err := b.CreateFolder(ctx, "Same"); err != nil {
		t.Fatal(err)
	}
	direct, _, err := b.CreateNote(ctx, "Same/Child.md", "direct subtree bytes\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, b, remoteB, 4)
	syncTimes(t, ctx, a, remoteA, 2)
	recoveredA := findNoteByContent(t, rootA, "direct subtree bytes")
	recoveredB := findNoteByContent(t, rootB, "direct subtree bytes")
	relativeA := relativeTestPath(t, rootA, recoveredA)
	relativeB := relativeTestPath(t, rootB, recoveredB)
	if filepath.Base(recoveredA) != "Child.md" || relativeA != relativeB {
		t.Fatalf("direct subtree paths A=%s B=%s", relativeA, relativeB)
	}
	if id := inspectTestNoteID(t, recoveredA); id != direct.ID {
		t.Fatalf("direct A id=%s want=%s", id, direct.ID)
	}
	if id := inspectTestNoteID(t, recoveredB); id != direct.ID {
		t.Fatalf("direct B id=%s want=%s", id, direct.ID)
	}
	remoteC := remote(t, server.URL, login(t, server.URL, email, password, "Matrix C"))
	rootC := t.TempDir()
	c, _, err := clientapp.Initialize(ctx, rootC)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	syncTimes(t, ctx, c, remoteC, 1)
	for _, name := range []string{"MoveDelete.md", "LocallyMoved.md", "DeleteMove.md", "RemotelyMoved.md"} {
		if _, err := c.ReadNote(ctx, name); err == nil {
			t.Fatalf("cold client revived tombstoned identity at %s", name)
		}
	}
	assertConflictContent(t, rootC, "edited local move must survive delete")
	assertConflictContent(t, rootC, "remote move must survive local delete")
	recoveredC := findNoteByContent(t, rootC, "direct subtree bytes")
	relativeC := relativeTestPath(t, rootC, recoveredC)
	if relativeC != relativeA {
		t.Fatalf("cold direct subtree path=%s want=%s", relativeC, relativeA)
	}
	if id := inspectTestNoteID(t, recoveredC); id != direct.ID {
		t.Fatalf("direct C id=%s want=%s", id, direct.ID)
	}
	_ = moveDelete
	_ = deleteMove
}

func TestAuthenticatedPostADR44Convergence(t *testing.T) {
	ctx := context.Background()
	server, err := integrationtest.New(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	const email, password = "post-adr44@example.test", "correct horse battery staple"
	if err := server.CreateVerifiedUser(ctx, email, password); err != nil {
		t.Fatal(err)
	}
	remoteA := remote(t, server.URL, login(t, server.URL, email, password, "Post ADR A"))
	remoteB := remote(t, server.URL, login(t, server.URL, email, password, "Post ADR B"))
	rootA, rootB := t.TempDir(), t.TempDir()
	a, _, err := clientapp.Initialize(ctx, rootA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, _, err := clientapp.Initialize(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if _, err := a.CreateFolder(ctx, "FolderMoveDelete"); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	states := httpSyncStates(t, ctx, remoteA)
	var originalFolderID uuid.UUID
	originalCount := 0
	for id, state := range states {
		if state.ObjectType == clientsync.Folder && state.Name == "FolderMoveDelete" && !state.Deleted {
			originalFolderID = id
			originalCount++
		}
	}
	if originalFolderID == uuid.Nil || originalCount != 1 {
		t.Fatalf("original folder candidates=%d id=%s", originalCount, originalFolderID)
	}
	if err := os.Remove(filepath.Join(rootA, "FolderMoveDelete")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	if err := os.Rename(filepath.Join(rootB, "FolderMoveDelete"), filepath.Join(rootB, "LocallyMovedFolder")); err != nil {
		t.Fatal(err)
	}
	reconcileFolderMove(t, ctx, rootB, "FolderMoveDelete")
	syncTimes(t, ctx, b, remoteB, 4)
	syncTimes(t, ctx, a, remoteA, 2)
	states = httpSyncStates(t, ctx, remoteA)
	original := states[originalFolderID]
	if !original.Deleted || original.Revision != 2 || original.Mutation != clientsync.Delete {
		t.Fatalf("original folder tombstone=%#v", original)
	}
	var recoveredFolderID uuid.UUID
	recoveredName := ""
	recoveredCount := 0
	for id, state := range states {
		if state.ObjectType == clientsync.Folder && !state.Deleted && state.ParentID != nil && *state.ParentID == clientsync.ConflictRecoveredID && strings.HasPrefix(state.Name, "LocallyMovedFolder (Konflikt - ") {
			recoveredFolderID, recoveredName = id, state.Name
			recoveredCount++
		}
	}
	if recoveredFolderID == uuid.Nil || recoveredFolderID == originalFolderID || recoveredCount != 1 {
		t.Fatalf("recovered candidates=%d id=%s original=%s", recoveredCount, recoveredFolderID, originalFolderID)
	}
	for label, root := range map[string]string{"A": rootA, "B": rootB} {
		if info, err := os.Stat(filepath.Join(root, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, recoveredName)); err != nil || !info.IsDir() {
			t.Fatalf("%s recovered folder missing: %v", label, err)
		}
		if _, err := os.Stat(filepath.Join(root, "LocallyMovedFolder")); !os.IsNotExist(err) {
			t.Fatalf("%s retained losing folder: %v", label, err)
		}
	}

	rootNote, _, err := a.CreateNote(ctx, "EquivalentRoot.md", "root equivalent base\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	for _, core := range []*clientapp.LocalCore{a, b} {
		doc, err := core.ReadNote(ctx, "EquivalentRoot.md")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := core.MoveNote(ctx, "EquivalentRoot.md", "EquivalentRootTarget.md", doc.Revision); err != nil {
			t.Fatal(err)
		}
	}
	rootLocal, err := b.ReadNote(ctx, "EquivalentRootTarget.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.SaveNote(ctx, "EquivalentRootTarget.md", rootLocal.Revision, "authenticated root dependent edit\n", nil); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 2)
	syncTimes(t, ctx, a, remoteA, 1)

	if _, err := a.CreateFolder(ctx, "KnownParent"); err != nil {
		t.Fatal(err)
	}
	nestedNote, _, err := a.CreateNote(ctx, "KnownParent/EquivalentNested.md", "nested equivalent base\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	for _, core := range []*clientapp.LocalCore{a, b} {
		doc, err := core.ReadNote(ctx, "KnownParent/EquivalentNested.md")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := core.MoveNote(ctx, "KnownParent/EquivalentNested.md", "KnownParent/EquivalentNestedTarget.md", doc.Revision); err != nil {
			t.Fatal(err)
		}
	}
	nestedLocal, err := b.ReadNote(ctx, "KnownParent/EquivalentNestedTarget.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.SaveNote(ctx, "KnownParent/EquivalentNestedTarget.md", nestedLocal.Revision, "authenticated nested dependent edit\n", nil); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 2)
	syncTimes(t, ctx, a, remoteA, 1)
	if copies, _ := filepath.Glob(filepath.Join(rootB, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "*.md")); len(copies) != 0 {
		t.Fatalf("equivalent moves created note conflict copies: %v", copies)
	}

	remoteC := remote(t, server.URL, login(t, server.URL, email, password, "Post ADR C"))
	rootC := t.TempDir()
	c, _, err := clientapp.Initialize(ctx, rootC)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	syncTimes(t, ctx, c, remoteC, 1)
	for label, core := range map[string]*clientapp.LocalCore{"A": a, "B": b, "C": c} {
		rootDoc, err := core.ReadNote(ctx, "EquivalentRootTarget.md")
		if err != nil || rootDoc.ID != rootNote.ID || rootDoc.Body != "authenticated root dependent edit\n" {
			t.Fatalf("%s root note=%#v err=%v", label, rootDoc, err)
		}
		nestedDoc, err := core.ReadNote(ctx, "KnownParent/EquivalentNestedTarget.md")
		if err != nil || nestedDoc.ID != nestedNote.ID || nestedDoc.Body != "authenticated nested dependent edit\n" {
			t.Fatalf("%s nested note=%#v err=%v", label, nestedDoc, err)
		}
		if copies, _ := filepath.Glob(filepath.Join(core.Root(), clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "*.md")); len(copies) != 0 {
			t.Fatalf("%s has unnecessary note conflicts: %v", label, copies)
		}
	}
	if info, err := os.Stat(filepath.Join(rootC, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, recoveredName)); err != nil || !info.IsDir() {
		t.Fatalf("cold C recovered folder missing: %v", err)
	}
}

func TestAuthenticatedADR49To51Convergence(t *testing.T) {
	ctx := context.Background()
	server, err := integrationtest.New(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	const email, password = "adr49-51@example.test", "correct horse battery staple"
	if err := server.CreateVerifiedUser(ctx, email, password); err != nil {
		t.Fatal(err)
	}
	remoteA := remote(t, server.URL, login(t, server.URL, email, password, "ADR49-51 A"))
	remoteB := remote(t, server.URL, login(t, server.URL, email, password, "ADR49-51 B"))
	rootA, rootB := t.TempDir(), t.TempDir()
	a, _, err := clientapp.Initialize(ctx, rootA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, _, err := clientapp.Initialize(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// ADR 0049: the canonical same-parent move keeps the original identity,
	// while B's different target and dependent edit become one visible copy.
	if _, err := a.CreateFolder(ctx, "DivergentParent"); err != nil {
		t.Fatal(err)
	}
	divergentNote, _, err := a.CreateNote(ctx, "DivergentParent/N.md", "authenticated nested winner bytes\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	winnerBytes, err := os.ReadFile(filepath.Join(rootA, "DivergentParent", "N.md"))
	if err != nil {
		t.Fatal(err)
	}
	aDoc, err := a.ReadNote(ctx, "DivergentParent/N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.MoveNote(ctx, "DivergentParent/N.md", "DivergentParent/Remote.md", aDoc.Revision); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	bDoc, err := b.ReadNote(ctx, "DivergentParent/N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.MoveNote(ctx, "DivergentParent/N.md", "DivergentParent/Local.md", bDoc.Revision); err != nil {
		t.Fatal(err)
	}
	bLocal, err := b.ReadNote(ctx, "DivergentParent/Local.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.SaveNote(ctx, "DivergentParent/Local.md", bLocal.Revision, "authenticated nested losing edit\n", []string{"nested"}); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, b, remoteB, 4)
	syncTimes(t, ctx, a, remoteA, 2)
	rootCopies, _ := filepath.Glob(filepath.Join(rootB, clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "*.md"))
	if len(rootCopies) != 1 {
		t.Fatalf("divergent recovered candidates=%v", rootCopies)
	}
	divergentRecoveredBytes, err := os.ReadFile(rootCopies[0])
	if err != nil {
		t.Fatal(err)
	}
	divergentRecoveredInspection, err := frontmatter.Inspect(divergentRecoveredBytes)
	if err != nil || divergentRecoveredInspection.NoteID == divergentNote.ID {
		t.Fatalf("divergent recovery identity=%#v err=%v", divergentRecoveredInspection, err)
	}
	divergentRecoveredID := divergentRecoveredInspection.NoteID
	divergentRecoveredName := filepath.Base(rootCopies[0])
	divergentRecoveredDoc, err := frontmatter.Read(divergentRecoveredBytes)
	if err != nil || string(divergentRecoveredDoc.Body) != "authenticated nested losing edit\n" {
		t.Fatalf("divergent recovered body=%q err=%v", divergentRecoveredDoc.Body, err)
	}

	// ADR 0050: a colliding Folder Create with two never-attempted Updates is
	// rekeyed under the reserved parent, preserving the Note UUID/final bytes.
	if _, err := a.CreateFolder(ctx, "EditedCreate"); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	if _, err := b.CreateFolder(ctx, "EditedCreate"); err != nil {
		t.Fatal(err)
	}
	editedCreateOriginalFolderID := localFolderIDAtPath(t, ctx, rootB, "EditedCreate")
	editedCreateNote, _, err := b.CreateNote(ctx, "EditedCreate/Child.md", "edited create v0\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	editedDoc, err := b.ReadNote(ctx, "EditedCreate/Child.md")
	if err != nil {
		t.Fatal(err)
	}
	editedDoc, _, err = b.SaveNote(ctx, "EditedCreate/Child.md", editedDoc.Revision, "edited create v1\n", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.SaveNote(ctx, "EditedCreate/Child.md", editedDoc.Revision, "edited create final\n", []string{"one", "two"}); err != nil {
		t.Fatal(err)
	}
	editedCreateBytes, err := os.ReadFile(filepath.Join(rootB, "EditedCreate", "Child.md"))
	if err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, b, remoteB, 4)
	syncTimes(t, ctx, a, remoteA, 2)

	// ADR 0051: A's exact tombstone wins, while B's moved Folder plus direct
	// Note Create and two Updates are recovered under a new Folder identity.
	if _, err := a.CreateFolder(ctx, "MoveDeleteNotes"); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	states := httpSyncStates(t, ctx, remoteA)
	moveDeleteFolderID, moveDeleteCount := uuid.Nil, 0
	for id, state := range states {
		if state.ObjectType == clientsync.Folder && !state.Deleted && state.ParentID == nil && state.Name == "MoveDeleteNotes" {
			moveDeleteFolderID, moveDeleteCount = id, moveDeleteCount+1
		}
	}
	if moveDeleteFolderID == uuid.Nil || moveDeleteCount != 1 {
		t.Fatalf("move/delete source candidates=%d id=%s", moveDeleteCount, moveDeleteFolderID)
	}
	if err := os.Remove(filepath.Join(rootA, "MoveDeleteNotes")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	if err := os.Rename(filepath.Join(rootB, "MoveDeleteNotes"), filepath.Join(rootB, "LocallyMovedWithNotes")); err != nil {
		t.Fatal(err)
	}
	reconcileFolderMove(t, ctx, rootB, "MoveDeleteNotes")
	movedNote, _, err := b.CreateNote(ctx, "LocallyMovedWithNotes/Child.md", "move delete v0\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	movedDoc, err := b.ReadNote(ctx, "LocallyMovedWithNotes/Child.md")
	if err != nil {
		t.Fatal(err)
	}
	movedDoc, _, err = b.SaveNote(ctx, "LocallyMovedWithNotes/Child.md", movedDoc.Revision, "move delete v1\n", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.SaveNote(ctx, "LocallyMovedWithNotes/Child.md", movedDoc.Revision, "move delete final\n", []string{"one", "two"}); err != nil {
		t.Fatal(err)
	}
	moveDeleteBytes, err := os.ReadFile(filepath.Join(rootB, "LocallyMovedWithNotes", "Child.md"))
	if err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, b, remoteB, 5)
	syncTimes(t, ctx, a, remoteA, 2)

	states = httpSyncStates(t, ctx, remoteA)
	originalMoveDelete := states[moveDeleteFolderID]
	if !originalMoveDelete.Deleted || originalMoveDelete.Mutation != clientsync.Delete || originalMoveDelete.Revision != 2 || originalMoveDelete.ObjectType != clientsync.Folder || originalMoveDelete.ParentID != nil || originalMoveDelete.Name != "MoveDeleteNotes" || len(originalMoveDelete.BlobHash) != 0 {
		t.Fatalf("move/delete tombstone=%#v", originalMoveDelete)
	}
	findRecoveredFolder := func(prefix string) (uuid.UUID, string) {
		t.Helper()
		id, name, count := uuid.Nil, "", 0
		for candidateID, state := range states {
			if state.ObjectType == clientsync.Folder && !state.Deleted && state.ParentID != nil && *state.ParentID == clientsync.ConflictRecoveredID && strings.HasPrefix(state.Name, prefix) {
				id, name, count = candidateID, state.Name, count+1
			}
		}
		if id == uuid.Nil || count != 1 {
			t.Fatalf("recovered folder %q candidates=%d id=%s", prefix, count, id)
		}
		return id, name
	}
	editedCreateFolderID, editedCreateFolderName := findRecoveredFolder("EditedCreate (Konflikt - ")
	if editedCreateFolderID == editedCreateOriginalFolderID {
		t.Fatalf("edited create recovery reused source id=%s", editedCreateOriginalFolderID)
	}
	moveDeleteRecoveredID, moveDeleteRecoveredName := findRecoveredFolder("LocallyMovedWithNotes (Konflikt - ")
	if moveDeleteRecoveredID == moveDeleteFolderID {
		t.Fatalf("move/delete recovery reused tombstoned id=%s", moveDeleteFolderID)
	}
	editedCreateState := states[editedCreateNote.ID]
	if editedCreateState.Deleted || editedCreateState.ObjectType != clientsync.Note || editedCreateState.ParentID == nil || *editedCreateState.ParentID != editedCreateFolderID || !bytes.Equal(editedCreateState.BlobHash, hashBytesForTest(editedCreateBytes)) {
		t.Fatalf("edited create state=%#v", editedCreateState)
	}
	movedNoteState := states[movedNote.ID]
	if movedNoteState.Deleted || movedNoteState.ObjectType != clientsync.Note || movedNoteState.ParentID == nil || *movedNoteState.ParentID != moveDeleteRecoveredID || !bytes.Equal(movedNoteState.BlobHash, hashBytesForTest(moveDeleteBytes)) {
		t.Fatalf("move/delete note state=%#v", movedNoteState)
	}
	findRootFolder := func(name string) uuid.UUID {
		t.Helper()
		id, count := uuid.Nil, 0
		for candidate, state := range states {
			if state.ObjectType == clientsync.Folder && !state.Deleted && state.ParentID == nil && state.Name == name {
				id, count = candidate, count+1
			}
		}
		if id == uuid.Nil || count != 1 {
			t.Fatalf("root folder %q candidates=%d id=%s", name, count, id)
		}
		return id
	}
	divergentParentID := findRootFolder("DivergentParent")
	editedCanonicalFolderID := findRootFolder("EditedCreate")
	canonicalState := states[divergentNote.ID]
	if canonicalState.Deleted || canonicalState.ObjectType != clientsync.Note || canonicalState.Mutation != clientsync.Move || canonicalState.ParentID == nil || *canonicalState.ParentID != divergentParentID || canonicalState.Name != "Remote.md" || canonicalState.Revision != 2 || !bytes.Equal(canonicalState.BlobHash, hashBytesForTest(winnerBytes)) {
		t.Fatalf("divergent canonical state=%#v", canonicalState)
	}
	recoveredState := states[divergentRecoveredID]
	if recoveredState.Deleted || recoveredState.ObjectType != clientsync.Note || recoveredState.Mutation != clientsync.Create || recoveredState.Revision != 1 || recoveredState.ParentID == nil || *recoveredState.ParentID != clientsync.ConflictRecoveredID || recoveredState.Name != divergentRecoveredName || !bytes.Equal(recoveredState.BlobHash, hashBytesForTest(divergentRecoveredBytes)) {
		t.Fatalf("divergent recovered state=%#v", recoveredState)
	}

	remoteC := remote(t, server.URL, login(t, server.URL, email, password, "ADR49-51 C"))
	rootC := t.TempDir()
	c, _, err := clientapp.Initialize(ctx, rootC)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	syncTimes(t, ctx, c, remoteC, 1)
	for label, core := range map[string]*clientapp.LocalCore{"A": a, "B": b, "C": c} {
		canonicalBytes, err := os.ReadFile(filepath.Join(core.Root(), "DivergentParent", "Remote.md"))
		if err != nil || !bytes.Equal(canonicalBytes, winnerBytes) || inspectTestNoteID(t, filepath.Join(core.Root(), "DivergentParent", "Remote.md")) != divergentNote.ID {
			t.Fatalf("%s divergent canonical exact=%t err=%v", label, bytes.Equal(canonicalBytes, winnerBytes), err)
		}
		recoveredPath := filepath.Join(core.Root(), clientsync.ConflictRootName, clientsync.ConflictRecoveredName, divergentRecoveredName)
		recoveredBytes, err := os.ReadFile(recoveredPath)
		if err != nil || !bytes.Equal(recoveredBytes, divergentRecoveredBytes) || inspectTestNoteID(t, recoveredPath) != divergentRecoveredID {
			t.Fatalf("%s divergent recovery exact=%t err=%v", label, bytes.Equal(recoveredBytes, divergentRecoveredBytes), err)
		}
		rootConflictCopies, _ := filepath.Glob(filepath.Join(core.Root(), clientsync.ConflictRootName, clientsync.ConflictRecoveredName, "*.md"))
		if len(rootConflictCopies) != 1 {
			t.Fatalf("%s divergent root conflict copies=%v", label, rootConflictCopies)
		}
		editedPath := filepath.Join(core.Root(), clientsync.ConflictRootName, clientsync.ConflictRecoveredName, editedCreateFolderName, "Child.md")
		editedBytes, err := os.ReadFile(editedPath)
		if err != nil || !bytes.Equal(editedBytes, editedCreateBytes) || inspectTestNoteID(t, editedPath) != editedCreateNote.ID {
			t.Fatalf("%s edited create exact=%t err=%v", label, bytes.Equal(editedBytes, editedCreateBytes), err)
		}
		movedPath := filepath.Join(core.Root(), clientsync.ConflictRootName, clientsync.ConflictRecoveredName, moveDeleteRecoveredName, "Child.md")
		movedBytes, err := os.ReadFile(movedPath)
		if err != nil || !bytes.Equal(movedBytes, moveDeleteBytes) || inspectTestNoteID(t, movedPath) != movedNote.ID {
			t.Fatalf("%s move/delete exact=%t err=%v", label, bytes.Equal(movedBytes, moveDeleteBytes), err)
		}
		if _, err := os.Stat(filepath.Join(core.Root(), "LocallyMovedWithNotes")); !os.IsNotExist(err) {
			t.Fatalf("%s retained losing move/delete path: %v", label, err)
		}
		if _, err := os.Stat(filepath.Join(core.Root(), "MoveDeleteNotes")); !os.IsNotExist(err) {
			t.Fatalf("%s materialized tombstoned folder: %v", label, err)
		}
		for relative, expected := range map[string]uuid.UUID{
			"DivergentParent": divergentParentID, "EditedCreate": editedCanonicalFolderID,
			filepath.ToSlash(filepath.Join(clientsync.ConflictRootName, clientsync.ConflictRecoveredName, editedCreateFolderName)):  editedCreateFolderID,
			filepath.ToSlash(filepath.Join(clientsync.ConflictRootName, clientsync.ConflictRecoveredName, moveDeleteRecoveredName)): moveDeleteRecoveredID,
		} {
			if got := localFolderIDAtPath(t, ctx, core.Root(), relative); got != expected {
				t.Fatalf("%s folder %s id=%s want=%s", label, relative, got, expected)
			}
		}
	}
}

func hashBytesForTest(content []byte) []byte {
	hash := sha256.Sum256(content)
	return hash[:]
}

func localFolderIDAtPath(t *testing.T, ctx context.Context, root, relative string) uuid.UUID {
	t.Helper()
	index, err := localindex.Open(ctx, filepath.Join(root, ".remember", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	snapshot, err := index.ReadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, count := uuid.Nil, 0
	for _, object := range snapshot.Objects {
		if object.Type == localindex.ObjectFolder && object.RelativePath == relative {
			id, count = object.ID, count+1
		}
	}
	if id == uuid.Nil || count != 1 {
		t.Fatalf("local folder %q candidates=%d id=%s", relative, count, id)
	}
	return id
}

func reconcileFolderMove(t *testing.T, ctx context.Context, root, source string) {
	t.Helper()
	index, err := localindex.Open(ctx, filepath.Join(root, ".remember", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if _, err := reconcile.Run(ctx, root, index, reconcile.Options{MoveCandidates: []string{source}}); err != nil {
		t.Fatal(err)
	}
}

func httpSyncStates(t *testing.T, ctx context.Context, remote *remotehttp.Client) map[uuid.UUID]clientsync.Change {
	t.Helper()
	states := map[uuid.UUID]clientsync.Change{}
	after := uint64(0)
	for page := 0; page < 16; page++ {
		result, err := remote.Pull(ctx, after, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, change := range result.Changes {
			states[change.ObjectID] = change
		}
		if !result.HasMore {
			return states
		}
		if result.NextCursor <= after {
			t.Fatal("non-progressing integration pull")
		}
		after = result.NextCursor
	}
	t.Fatal("integration pull page bound exceeded")
	return nil
}

func assertConflictContent(t *testing.T, root, needle string) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(root, "_Konflikte", "Wiederhergestellt", "*.md"))
	for _, match := range matches {
		content, err := os.ReadFile(match)
		if err == nil && bytes.Contains(content, []byte(needle)) {
			return
		}
	}
	t.Fatalf("conflict content %q absent in %v", needle, matches)
}
func relativeTestPath(t *testing.T, root, absolute string) string {
	t.Helper()
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		t.Fatalf("invalid test path %s: %v", absolute, err)
	}
	return filepath.ToSlash(relative)
}
func inspectTestNoteID(t *testing.T, absolute string) uuid.UUID {
	t.Helper()
	content, err := os.ReadFile(absolute)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := frontmatter.Inspect(content)
	if err != nil {
		t.Fatal(err)
	}
	return inspection.NoteID
}

func findNoteByContent(t *testing.T, root, needle string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == ".remember" {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if bytes.Contains(content, []byte(needle)) {
				if found != "" {
					return fmt.Errorf("duplicate content")
				}
				found = path
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestTwoClientsConvergeThroughAuthenticatedHTTP(t *testing.T) {
	ctx := context.Background()
	serverRoot := t.TempDir()
	server, err := integrationtest.New(ctx, serverRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	const email, password = "sync@example.test", "correct horse battery staple"
	if err := server.CreateVerifiedUser(ctx, email, password); err != nil {
		t.Fatal(err)
	}
	tokenA, tokenB := login(t, server.URL, email, password, "Device A"), login(t, server.URL, email, password, "Device B")
	remoteA, remoteB := remote(t, server.URL, tokenA), remote(t, server.URL, tokenB)
	rootA, rootB := t.TempDir(), t.TempDir()
	a, _, err := clientapp.Initialize(ctx, rootA)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, _, err := clientapp.Initialize(ctx, rootB)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	created, _, err := a.CreateNote(ctx, "N.md", "base\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	stale, err := b.ReadNote(ctx, "N.md")
	if err != nil || stale.ID != created.ID {
		t.Fatalf("initial pull=%#v err=%v", stale, err)
	}
	current, err := a.ReadNote(ctx, "N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.SaveNote(ctx, "N.md", current.Revision, "canonical from A\n", nil); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	if _, _, err := b.SaveNote(ctx, "N.md", stale.Revision, "conflicting from B\n", nil); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, b, remoteB, 3)
	syncTimes(t, ctx, a, remoteA, 2)
	for label, core := range map[string]*clientapp.LocalCore{"A": a, "B": b} {
		note, err := core.ReadNote(ctx, "N.md")
		if err != nil || !strings.Contains(note.Body, "canonical from A") {
			t.Fatalf("%s canonical=%#v err=%v", label, note, err)
		}
	}
	copiesA, _ := filepath.Glob(filepath.Join(rootA, "_Konflikte", "Wiederhergestellt", "N (Konflikt *).md"))
	copiesB, _ := filepath.Glob(filepath.Join(rootB, "_Konflikte", "Wiederhergestellt", "N (Konflikt *).md"))
	if len(copiesA) != 1 || len(copiesB) != 1 || filepath.Base(copiesA[0]) != filepath.Base(copiesB[0]) {
		t.Fatalf("conflict copies A=%v B=%v", copiesA, copiesB)
	}
	contentA, errA := os.ReadFile(copiesA[0])
	contentB, errB := os.ReadFile(copiesB[0])
	if errA != nil || errB != nil || !bytes.Equal(contentA, contentB) || !bytes.Contains(contentA, []byte("conflicting from B")) {
		t.Fatalf("conflict convergence errA=%v errB=%v equal=%t", errA, errB, bytes.Equal(contentA, contentB))
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	server, err = integrationtest.New(ctx, serverRoot)
	if err != nil {
		t.Fatal(err)
	}
	remoteA, remoteB = remote(t, server.URL, tokenA), remote(t, server.URL, tokenB)
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	remoteC := remote(t, server.URL, login(t, server.URL, email, password, "Device C"))
	rootC := t.TempDir()
	c, _, err := clientapp.Initialize(ctx, rootC)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	syncTimes(t, ctx, c, remoteC, 1)
	cold, err := c.ReadNote(ctx, "N.md")
	if err != nil || !strings.Contains(cold.Body, "canonical from A") {
		t.Fatalf("cold bootstrap canonical=%#v err=%v", cold, err)
	}
	coldCopies, _ := filepath.Glob(filepath.Join(rootC, "_Konflikte", "Wiederhergestellt", "N (Konflikt *).md"))
	if len(coldCopies) != 1 {
		t.Fatalf("cold bootstrap conflict copies=%v", coldCopies)
	}
	if _, err := a.CreateFolder(ctx, "Archive"); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	canonical, err := a.ReadNote(ctx, "N.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.MoveNote(ctx, "N.md", "Archive/N.md", canonical.Revision); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	moved, err := b.ReadNote(ctx, "Archive/N.md")
	if err != nil || !strings.Contains(moved.Body, "canonical from A") {
		t.Fatalf("note move did not converge: %#v err=%v", moved, err)
	}
	if _, err := b.DeleteNote(ctx, "Archive/N.md", moved.Revision); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, b, remoteB, 1)
	syncTimes(t, ctx, a, remoteA, 1)
	for label, core := range map[string]*clientapp.LocalCore{"A": a, "B": b} {
		if _, err := core.ReadNote(ctx, "Archive/N.md"); err == nil {
			t.Fatalf("%s note delete did not converge", label)
		}
	}
	if _, err := a.CreateFolder(ctx, "MoveMe"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.CreateNote(ctx, "MoveMe/Inside.md", "inside\n", nil); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	if err := os.Rename(filepath.Join(rootA, "MoveMe"), filepath.Join(rootA, "Moved")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	if info, err := os.Stat(filepath.Join(rootB, "Moved")); err != nil || !info.IsDir() {
		t.Fatalf("folder move did not converge: %v", err)
	}
	inside, err := a.ReadNote(ctx, "Moved/Inside.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DeleteNote(ctx, "Moved/Inside.md", inside.Revision); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	if err := os.Remove(filepath.Join(rootA, "Moved")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	syncTimes(t, ctx, a, remoteA, 1)
	syncTimes(t, ctx, b, remoteB, 1)
	if _, err := os.Stat(filepath.Join(rootB, "Moved")); !os.IsNotExist(err) {
		t.Fatalf("folder delete did not converge: %v", err)
	}
	syncTimes(t, ctx, c, remoteC, 1)
	if _, err := c.ReadNote(ctx, "Archive/N.md"); err == nil {
		t.Fatal("cold client retained deleted note")
	}
	if _, err := os.Stat(filepath.Join(rootC, "Moved")); !os.IsNotExist(err) {
		t.Fatalf("cold client retained deleted folder: %v", err)
	}
	if _, err := a.CreateFolder(ctx, "Bulk"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 110; i++ {
		name := fmt.Sprintf("Bulk/N%03d.md", i)
		if _, _, err := a.CreateNote(ctx, name, fmt.Sprintf("bulk %03d\n", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	syncTimes(t, ctx, a, remoteA, 1)
	remoteD := remote(t, server.URL, login(t, server.URL, email, password, "Device D"))
	rootD := t.TempDir()
	d, _, err := clientapp.Initialize(ctx, rootD)
	if err != nil {
		t.Fatal(err)
	}
	interrupted := &interruptingRemote{Client: remoteD}
	if err := d.SyncOnce(ctx, interrupted); err == nil || !strings.Contains(err.Error(), "pull-page interruption") {
		t.Fatalf("interrupted page sync err=%v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, _, err = clientapp.Open(ctx, rootD)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	resumedRemote := &recordingRemote{Client: remoteD}
	if err := d.SyncOnce(ctx, resumedRemote); err != nil {
		t.Fatal(err)
	}
	if len(interrupted.afters) != 2 || interrupted.firstNext == 0 || interrupted.afters[1] != interrupted.firstNext || len(resumedRemote.afters) == 0 || resumedRemote.afters[0] != interrupted.firstNext {
		t.Fatalf("pull resume interrupted=%v firstNext=%d resumed=%v", interrupted.afters, interrupted.firstNext, resumedRemote.afters)
	}
	for _, name := range []string{"Bulk/N000.md", "Bulk/N109.md"} {
		note, err := d.ReadNote(ctx, name)
		if err != nil || !strings.Contains(note.Body, "bulk") {
			t.Fatalf("paged note %s=%#v err=%v", name, note, err)
		}
	}
	if _, err := d.ReadNote(ctx, "Archive/N.md"); err == nil {
		t.Fatal("paged client retained deleted note")
	}
}
