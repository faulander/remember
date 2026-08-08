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
	"github.com/faulander/remember/client/internal/remotehttp"
	"github.com/faulander/remember/server/integrationtest"
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
