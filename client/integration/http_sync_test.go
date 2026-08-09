package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
