package remotehttp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/faulander/remember/client/internal/clientsync"
	"github.com/google/uuid"
)

func tokenSource() AccessTokenSource {
	return AccessTokenSourceFunc(func(context.Context) (string, error) { return "secret-access", nil })
}

func TestClientSubmitPullAndBlobContracts(t *testing.T) {
	object := uuid.New()
	op := uuid.Must(uuid.NewV7())
	blob := []byte("blob")
	hash := sha256.Sum256(blob)
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-access" {
			t.Error("missing bearer")
		}
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		switch r.URL.Path {
		case "/v1/blobs/" + fmtHash(hash):
			if r.Method == http.MethodPut {
				b, _ := io.ReadAll(r.Body)
				if string(b) != "blob" || r.Header.Get("Content-Type") != "application/octet-stream" {
					t.Error("bad blob put")
				}
				jsonResponse(w, map[string]any{"hash": fmtHash(hash), "size": 4})
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", "4")
			w.Write(blob)
		case "/v1/sync/operations":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["operation_id"] != op.String() {
				t.Error("operation changed")
			}
			jsonResponse(w, map[string]any{"accepted": true, "conflict": nil, "revision": 1, "cursor": 1, "canonical": nil})
		case "/v1/sync/changes":
			if r.URL.RawQuery != "after=0&limit=100" {
				t.Errorf("query=%s", r.URL.RawQuery)
			}
			jsonResponse(w, map[string]any{"changes": []any{map[string]any{"cursor": 1, "mutation": "create", "operation_id": op.String(), "object_id": object.String(), "object_type": "note", "revision": 1, "parent_id": nil, "name": "N.md", "blob_hash": fmtHash(hash), "deleted": false}}, "has_more": false, "next_cursor": 1})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, nil, tokenSource())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.PutBlob(context.Background(), hash, blob); err != nil {
		t.Fatal(err)
	}
	if got, err := client.ResolveBlob(context.Background(), hash); err != nil || string(got) != "blob" {
		t.Fatalf("blob=%q err=%v", got, err)
	}
	result, err := client.Submit(context.Background(), clientsync.Mutation{OperationID: op, Kind: clientsync.Create, ObjectID: object, ObjectType: clientsync.Note, Name: "N.md", BlobHash: hash[:]})
	if err != nil || !result.Accepted || result.Revision != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	page, err := client.Pull(context.Background(), 0, 100)
	if err != nil || len(page.Changes) != 1 || page.Changes[0].ObjectID != object {
		t.Fatalf("page=%#v err=%v", page, err)
	}
	if len(calls) != 4 {
		t.Fatalf("calls=%v", calls)
	}
}

func TestPreserveDeleteFolderContract(t *testing.T) {
	operation, conflict := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	folder, recovered := uuid.New(), uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sync/folder-preserve-delete" || r.Method != http.MethodPost {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["operation_id"] != operation.String() || body["conflict_operation_id"] != conflict.String() || body["folder_id"] != folder.String() || body["expected_revision"] != float64(2) {
			t.Errorf("body=%v", body)
		}
		jsonResponse(w, map[string]any{"recovered_folder_id": recovered.String(), "recovered_cursor": 10, "deleted_cursor": 11})
	}))
	defer server.Close()
	client, err := New(server.URL, nil, tokenSource())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.PreserveAndDeleteEmptyFolder(context.Background(), operation, conflict, folder, 2)
	if err != nil || result.RecoveredFolderID != recovered || result.RecoveredCursor != 10 || result.DeletedCursor != 11 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestResolveBlobClassifiesMissingReference(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"blob_not_found"}`))
	}))
	defer server.Close()
	client, err := New(server.URL, nil, tokenSource())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ResolveBlob(context.Background(), sha256.Sum256([]byte("missing"))); !errors.Is(err, clientsync.ErrBlobMissing) {
		t.Fatalf("missing err=%v", err)
	}
}

func TestClientRefusesRedirectDuplicateJSONAndNonTLSRemote(t *testing.T) {
	if _, err := New("http://example.com", nil, tokenSource()); err == nil {
		t.Fatal("non-TLS accepted")
	}
	forwarded := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded = r.Header.Get("Authorization") != "" }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, _ := New(server.URL, nil, tokenSource())
	op := uuid.Must(uuid.NewV7())
	_, err := client.Submit(context.Background(), clientsync.Mutation{OperationID: op, Kind: clientsync.Create, ObjectID: uuid.New(), ObjectType: clientsync.Folder, Name: "F"})
	if !errors.Is(err, ErrInvalidResponse) || forwarded {
		t.Fatalf("redirect err=%v forwarded=%t", err, forwarded)
	}
	duplicate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"accepted":true,"accepted":true,"conflict":null,"revision":1,"cursor":1}`)
	}))
	defer duplicate.Close()
	client, _ = New(duplicate.URL, nil, tokenSource())
	if _, err := client.Submit(context.Background(), clientsync.Mutation{OperationID: op, Kind: clientsync.Create, ObjectID: uuid.New(), ObjectType: clientsync.Folder, Name: "F"}); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("duplicate err=%v", err)
	}
}

func TestClientValidatesCanonicalConflictState(t *testing.T) {
	op := uuid.Must(uuid.NewV7())
	object := uuid.New()
	hash := sha256.Sum256([]byte("canonical"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		jsonResponse(w, map[string]any{"accepted": false, "conflict": "base_revision_mismatch", "revision": nil, "cursor": nil, "canonical": map[string]any{"object_type": "note", "revision": 2, "parent_id": nil, "name": "N.md", "blob_hash": fmtHash(hash), "deleted": false}})
	}))
	defer server.Close()
	client, _ := New(server.URL, nil, tokenSource())
	result, err := client.Submit(context.Background(), clientsync.Mutation{OperationID: op, Kind: clientsync.Update, ObjectID: object, ObjectType: clientsync.Note, BaseRevision: 1, BlobHash: hash[:]})
	if err != nil || result.Canonical == nil || result.Canonical.Revision != 2 || !bytes.Equal(result.Canonical.BlobHash, hash[:]) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	for _, omitted := range []string{"object_type", "revision", "parent_id", "name", "blob_hash", "deleted"} {
		t.Run("missing "+omitted, func(t *testing.T) {
			canonical := map[string]any{"object_type": "note", "revision": 2, "parent_id": nil, "name": "N.md", "blob_hash": fmtHash(hash), "deleted": false}
			delete(canonical, omitted)
			invalid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				jsonResponse(w, map[string]any{"accepted": false, "conflict": "base_revision_mismatch", "revision": nil, "cursor": nil, "canonical": canonical})
			}))
			defer invalid.Close()
			client, _ := New(invalid.URL, nil, tokenSource())
			if _, err := client.Submit(context.Background(), clientsync.Mutation{OperationID: uuid.Must(uuid.NewV7()), Kind: clientsync.Update, ObjectID: object, ObjectType: clientsync.Note, BaseRevision: 1, BlobHash: hash[:]}); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("missing %s accepted: %v", omitted, err)
			}
		})
	}
}

func TestClientRejectsNullScalarsInvalidMutationAndUnknownErrorCode(t *testing.T) {
	t.Run("null submit scalar", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"accepted":null,"conflict":"object_exists","revision":null,"cursor":null}`)
		}))
		defer server.Close()
		client, _ := New(server.URL, nil, tokenSource())
		mutation := clientsync.Mutation{OperationID: uuid.Must(uuid.NewV7()), Kind: clientsync.Create, ObjectID: uuid.New(), ObjectType: clientsync.Folder, Name: "F"}
		if _, err := client.Submit(context.Background(), mutation); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("null submit err=%v", err)
		}
	})
	t.Run("null pull fields", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"changes":null,"has_more":null,"next_cursor":0}`)
		}))
		defer server.Close()
		client, _ := New(server.URL, nil, tokenSource())
		if _, err := client.Pull(context.Background(), 0, 100); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("null pull err=%v", err)
		}
	})
	t.Run("invalid outbound mutation", func(t *testing.T) {
		called := false
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		defer server.Close()
		client, _ := New(server.URL, nil, tokenSource())
		if _, err := client.Submit(context.Background(), clientsync.Mutation{}); err == nil || called {
			t.Fatalf("invalid mutation err=%v called=%t", err, called)
		}
	})
	t.Run("token source error is redacted", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		defer server.Close()
		tokens := AccessTokenSourceFunc(func(context.Context) (string, error) { return "", errors.New("secret-access leaked") })
		client, _ := New(server.URL, nil, tokens)
		if _, err := client.Pull(context.Background(), 0, 100); err == nil || strings.Contains(err.Error(), "secret-access") {
			t.Fatalf("token source err=%v", err)
		}
	})
	t.Run("unknown error code", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"attacker_controlled"}`)
		}))
		defer server.Close()
		client, _ := New(server.URL, nil, tokenSource())
		mutation := clientsync.Mutation{OperationID: uuid.Must(uuid.NewV7()), Kind: clientsync.Create, ObjectID: uuid.New(), ObjectType: clientsync.Folder, Name: "F"}
		if _, err := client.Submit(context.Background(), mutation); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("unknown code err=%v", err)
		}
	})
}

func jsonResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
func fmtHash(hash [32]byte) string { return fmt.Sprintf("%x", hash[:]) }
