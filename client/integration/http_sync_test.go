package integration

import (
	"bytes"
	"context"
	"encoding/json"
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
	server, err := integrationtest.New(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	const email, password = "sync@example.test", "correct horse battery staple"
	if err := server.CreateVerifiedUser(ctx, email, password); err != nil {
		t.Fatal(err)
	}
	remoteA := remote(t, server.URL, login(t, server.URL, email, password, "Device A"))
	remoteB := remote(t, server.URL, login(t, server.URL, email, password, "Device B"))
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
}
