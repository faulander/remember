package remotehttp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	folder, recovered, originalChild, recoveredChild, note := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	hash := sha256.Sum256([]byte("note"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sync/folder-preserve-delete" || r.Method != http.MethodPost {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["operation_id"] != operation.String() || body["conflict_operation_id"] != conflict.String() || body["folder_id"] != folder.String() || body["expected_revision"] != float64(2) || body["request_version"] != float64(3) || body["known_cursor"] != float64(9) {
			t.Errorf("body=%v", body)
		}
		jsonResponse(w, map[string]any{"recovered_folder_id": recovered.String(), "recovered_folder_name": "Recovered", "recovered_cursor": 10, "deleted_cursor": 14, "first_cursor": 10, "last_cursor": 14, "clones": []map[string]any{{"original_folder_id": originalChild.String(), "recovered_folder_id": recoveredChild.String(), "create_cursor": 11, "delete_cursor": 13, "source_revision": 1, "name": "Empty"}}, "note_moves": []map[string]any{{"note_id": note.String(), "move_cursor": 12, "source_revision": 2, "target_revision": 3, "source_parent_id": folder.String(), "target_parent_id": recovered.String(), "name": "N.md", "blob_hash": hex.EncodeToString(hash[:])}}})
	}))
	defer server.Close()
	client, err := New(server.URL, nil, tokenSource())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.PreserveAndDeleteEmptyFolder(context.Background(), operation, conflict, folder, 2, 9, 3)
	if err != nil || result.RecoveredFolderID != recovered || result.RecoveredFolderName != "Recovered" || result.RecoveredCursor != 10 || result.DeletedCursor != 14 || len(result.Clones) != 1 || result.Clones[0].SourceRevision != 1 || result.Clones[0].Name != "Empty" || len(result.NoteMoves) != 1 || result.NoteMoves[0].NoteID != note {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestPreserveDeleteV4MixedDepthDAG(t *testing.T) {
	operation, conflict := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	folder := uuid.MustParse("10000000-0000-4000-8000-000000000001")
	recovered := uuid.MustParse("20000000-0000-4000-8000-000000000001")
	originals := []uuid.UUID{
		uuid.MustParse("30000000-0000-4000-8000-000000000001"),
		uuid.MustParse("30000000-0000-4000-8000-000000000002"),
		uuid.MustParse("30000000-0000-4000-8000-000000000003"),
		uuid.MustParse("30000000-0000-4000-8000-000000000004"),
	}
	targets := []uuid.UUID{
		uuid.MustParse("40000000-0000-4000-8000-000000000001"),
		uuid.MustParse("40000000-0000-4000-8000-000000000002"),
		uuid.MustParse("40000000-0000-4000-8000-000000000003"),
		uuid.MustParse("40000000-0000-4000-8000-000000000004"),
	}
	notes := []uuid.UUID{
		uuid.MustParse("50000000-0000-4000-8000-000000000001"),
		uuid.MustParse("50000000-0000-4000-8000-000000000002"),
	}
	hash := sha256.Sum256([]byte("exact"))
	response := map[string]any{
		"recovered_folder_id": recovered.String(), "recovered_folder_name": "Recovered", "recovered_cursor": 10, "deleted_cursor": 21, "first_cursor": 10, "last_cursor": 21,
		"clones": []map[string]any{
			{"original_folder_id": originals[0].String(), "recovered_folder_id": targets[0].String(), "source_parent_id": folder.String(), "target_parent_id": recovered.String(), "depth": 1, "create_cursor": 11, "delete_cursor": 19, "source_revision": 2, "name": "A"},
			{"original_folder_id": originals[1].String(), "recovered_folder_id": targets[1].String(), "source_parent_id": folder.String(), "target_parent_id": recovered.String(), "depth": 1, "create_cursor": 12, "delete_cursor": 20, "source_revision": 3, "name": "B"},
			{"original_folder_id": originals[2].String(), "recovered_folder_id": targets[2].String(), "source_parent_id": originals[0].String(), "target_parent_id": targets[0].String(), "depth": 2, "create_cursor": 13, "delete_cursor": 18, "source_revision": 4, "name": "C"},
			{"original_folder_id": originals[3].String(), "recovered_folder_id": targets[3].String(), "source_parent_id": originals[2].String(), "target_parent_id": targets[2].String(), "depth": 3, "create_cursor": 14, "delete_cursor": 17, "source_revision": 5, "name": "D"},
		},
		"note_moves": []map[string]any{
			{"note_id": notes[0].String(), "move_cursor": 15, "source_revision": 6, "target_revision": 7, "source_parent_id": folder.String(), "target_parent_id": recovered.String(), "name": "Root.md", "blob_hash": hex.EncodeToString(hash[:])},
			{"note_id": notes[1].String(), "move_cursor": 16, "source_revision": 8, "target_revision": 9, "source_parent_id": originals[2].String(), "target_parent_id": targets[2].String(), "name": "Nested.md", "blob_hash": hex.EncodeToString(hash[:])},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["request_version"] != float64(4) || body["known_cursor"] != float64(9) {
			t.Errorf("body=%v", body)
		}
		jsonResponse(w, response)
	}))
	defer server.Close()
	client, _ := New(server.URL, nil, tokenSource())
	result, err := client.PreserveAndDeleteEmptyFolder(context.Background(), operation, conflict, folder, 2, 9, 4)
	if err != nil || len(result.Clones) != 4 || result.Clones[3].Depth != 3 || result.Clones[3].TargetParentID != targets[2] || len(result.NoteMoves) != 2 || result.NoteMoves[1].TargetParentID != targets[2] {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestPreserveDeleteV4RejectsMalformedTreeMappings(t *testing.T) {
	operation, conflict := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	folder, recovered := uuid.New(), uuid.New()
	child, recoveredChild := uuid.New(), uuid.New()
	hash := sha256.Sum256([]byte("exact"))
	base := func() map[string]any {
		return map[string]any{
			"recovered_folder_id": recovered.String(), "recovered_folder_name": "Recovered", "recovered_cursor": 10, "deleted_cursor": 14, "first_cursor": 10, "last_cursor": 14,
			"clones":     []map[string]any{{"original_folder_id": child.String(), "recovered_folder_id": recoveredChild.String(), "source_parent_id": folder.String(), "target_parent_id": recovered.String(), "depth": 1, "create_cursor": 11, "delete_cursor": 13, "source_revision": 1, "name": "Child"}},
			"note_moves": []map[string]any{{"note_id": uuid.New().String(), "move_cursor": 12, "source_revision": 1, "target_revision": 2, "source_parent_id": child.String(), "target_parent_id": recoveredChild.String(), "name": "N.md", "blob_hash": hex.EncodeToString(hash[:])}},
		}
	}
	cases := map[string]func(map[string]any){
		"wrong target parent": func(out map[string]any) {
			out["clones"].([]map[string]any)[0]["target_parent_id"] = uuid.New().String()
		},
		"wrong depth":        func(out map[string]any) { out["clones"].([]map[string]any)[0]["depth"] = 2 },
		"root delete reused": func(out map[string]any) { out["clones"].([]map[string]any)[0]["delete_cursor"] = 14 },
		"note target mismatch": func(out map[string]any) {
			out["note_moves"].([]map[string]any)[0]["target_parent_id"] = recovered.String()
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			out := base()
			mutate(out)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { jsonResponse(w, out) }))
			defer server.Close()
			client, _ := New(server.URL, nil, tokenSource())
			if _, err := client.PreserveAndDeleteEmptyFolder(context.Background(), operation, conflict, folder, 2, 9, 4); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestPreserveDeleteV3RejectsV4CloneFieldsEvenWhenZero(t *testing.T) {
	operation, conflict := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	folder, recovered, child, recoveredChild := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		jsonResponse(w, map[string]any{
			"recovered_folder_id": recovered.String(), "recovered_folder_name": "Recovered", "recovered_cursor": 10, "deleted_cursor": 13, "first_cursor": 10, "last_cursor": 13,
			"clones":     []map[string]any{{"original_folder_id": child.String(), "recovered_folder_id": recoveredChild.String(), "source_parent_id": "", "target_parent_id": "", "depth": 0, "create_cursor": 11, "delete_cursor": 12, "source_revision": 1, "name": "Child"}},
			"note_moves": []any{},
		})
	}))
	defer server.Close()
	client, _ := New(server.URL, nil, tokenSource())
	if _, err := client.PreserveAndDeleteEmptyFolder(context.Background(), operation, conflict, folder, 2, 9, 3); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("err=%v", err)
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

func TestPreserveDeleteV1RequestAndV2ResponseRemainLegacy(t *testing.T) {
	operation, conflict := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	folder, recovered := uuid.New(), uuid.New()
	for _, version := range []uint64{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if version == 1 {
					if _, ok := body["request_version"]; ok {
						t.Errorf("v1 version leaked: %v", body)
					}
					jsonResponse(w, map[string]any{"recovered_folder_id": recovered.String(), "recovered_cursor": 10, "deleted_cursor": 11})
				} else {
					if body["request_version"] != float64(2) {
						t.Errorf("v2 body=%v", body)
					}
					jsonResponse(w, map[string]any{"recovered_folder_id": recovered.String(), "recovered_cursor": 10, "deleted_cursor": 11, "first_cursor": 10, "last_cursor": 11, "clones": []any{}})
				}
			}))
			defer server.Close()
			client, _ := New(server.URL, nil, tokenSource())
			known := uint64(0)
			if version == 2 {
				known = 9
			}
			result, err := client.PreserveAndDeleteEmptyFolder(context.Background(), operation, conflict, folder, 2, known, version)
			if err != nil || result.RecoveredFolderID != recovered {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}

func TestPreserveDeleteV3ResponseLimit(t *testing.T) {
	operation, conflict := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	folder := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"recovered_folder_id":"`+uuid.New().String()+`","recovered_folder_name":"Recovered","recovered_cursor":1,"deleted_cursor":2,"first_cursor":1,"last_cursor":2,"clones":[],"note_moves":[],"padding":"`+strings.Repeat("x", maxPreserveDeleteJSONBytes)+`"}`)
	}))
	defer server.Close()
	client, _ := New(server.URL, nil, tokenSource())
	if _, err := client.PreserveAndDeleteEmptyFolder(context.Background(), operation, conflict, folder, 2, 9, 3); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("oversize err=%v", err)
	}
}

func TestPreserveDeleteV4ResponseLimit(t *testing.T) {
	operation, conflict := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	folder, recovered := uuid.New(), uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"recovered_folder_id":"`+recovered.String()+`","recovered_folder_name":"Recovered","recovered_cursor":1,"deleted_cursor":2,"first_cursor":1,"last_cursor":2,"clones":[],"note_moves":[]}`)
		io.WriteString(w, strings.Repeat(" ", maxPreserveDeleteV4JSONBytes))
	}))
	defer server.Close()
	client, _ := New(server.URL, nil, tokenSource())
	if _, err := client.PreserveAndDeleteEmptyFolder(context.Background(), operation, conflict, folder, 2, 9, 4); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("oversize err=%v", err)
	}
}
