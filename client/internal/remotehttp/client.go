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
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/faulander/remember/client/internal/clientsync"
	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
)

const maxJSONBytes = 64 * 1024

var (
	ErrInvalidResponse = errors.New("invalid remote response")
	ErrReauthRequired  = errors.New("reauthentication required")
	ErrRetryable       = errors.New("remote request may be retried")
	ErrReplayMismatch  = errors.New("operation replay mismatch")
)

type AccessTokenSource interface {
	AccessToken(context.Context) (string, error)
}
type AccessTokenSourceFunc func(context.Context) (string, error)

func (f AccessTokenSourceFunc) AccessToken(ctx context.Context) (string, error) { return f(ctx) }

type Client struct {
	base   *url.URL
	http   *http.Client
	tokens AccessTokenSource
}

type RejectedError struct {
	Status int
	Code   string
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("remote request rejected with status %d", e.Status)
}

func validUUIDv7(id uuid.UUID) bool {
	return id.Version() == 7 && id.Variant() == uuid.RFC4122 && id != uuid.Nil
}

type PreserveDeleteFolderResult struct {
	RecoveredFolderID              uuid.UUID
	RecoveredCursor, DeletedCursor uint64
}

func (c *Client) PreserveAndDeleteEmptyFolder(ctx context.Context, operationID, conflictOperationID, folderID uuid.UUID, expectedRevision uint64) (PreserveDeleteFolderResult, error) {
	if !validUUIDv7(operationID) || !validUUIDv7(conflictOperationID) || folderID == uuid.Nil || folderID.Variant() != uuid.RFC4122 || expectedRevision == 0 || expectedRevision > math.MaxInt64 {
		return PreserveDeleteFolderResult{}, errors.New("invalid preserve delete request")
	}
	body, _ := json.Marshal(map[string]any{"operation_id": operationID.String(), "conflict_operation_id": conflictOperationID.String(), "folder_id": folderID.String(), "expected_revision": expectedRevision})
	resp, err := c.request(ctx, http.MethodPost, "/v1/sync/folder-preserve-delete", body, "application/json")
	if err != nil {
		return PreserveDeleteFolderResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return PreserveDeleteFolderResult{}, classify(resp)
	}
	var out struct {
		RecoveredFolderID string `json:"recovered_folder_id"`
		RecoveredCursor   uint64 `json:"recovered_cursor"`
		DeletedCursor     uint64 `json:"deleted_cursor"`
	}
	if err := decodeJSON(resp, &out, "recovered_folder_id", "recovered_cursor", "deleted_cursor"); err != nil {
		return PreserveDeleteFolderResult{}, ErrInvalidResponse
	}
	id, err := uuid.Parse(out.RecoveredFolderID)
	if err != nil || id == uuid.Nil || id.Variant() != uuid.RFC4122 || id == folderID || out.RecoveredCursor == 0 || out.RecoveredCursor >= math.MaxInt64 || out.DeletedCursor != out.RecoveredCursor+1 || out.DeletedCursor > math.MaxInt64 {
		return PreserveDeleteFolderResult{}, ErrInvalidResponse
	}
	return PreserveDeleteFolderResult{id, out.RecoveredCursor, out.DeletedCursor}, nil
}

type PullPage struct {
	Changes    []clientsync.Change
	HasMore    bool
	NextCursor uint64
}

func New(rawBase string, transport *http.Client, tokens AccessTokenSource) (*Client, error) {
	u, err := url.Parse(rawBase)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(u.Scheme == "http" && loopbackHost(u.Hostname()))) {
		return nil, errors.New("invalid remote base URL")
	}
	if tokens == nil {
		return nil, errors.New("nil access token source")
	}
	if transport == nil {
		transport = &http.Client{Timeout: 30 * time.Second}
	}
	clone := *transport
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if clone.Timeout == 0 {
		clone.Timeout = 30 * time.Second
	}
	u.Path = ""
	return &Client{base: u, http: &clone, tokens: tokens}, nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Client) request(ctx context.Context, method, path string, body []byte, contentType string) (*http.Response, error) {
	token, err := c.tokens.AccessToken(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("access token unavailable")
	}
	if token == "" || len(token) > 4096 || strings.ContainsAny(token, "\r\n\x00") {
		return nil, errors.New("invalid access token")
	}
	relative, err := url.Parse(path)
	if err != nil || relative.IsAbs() || !strings.HasPrefix(relative.Path, "/") {
		return nil, errors.New("invalid remote request path")
	}
	u := *c.base
	u.Path, u.RawQuery = relative.Path, relative.RawQuery
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("build remote request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: transport failure", ErrRetryable)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, ErrInvalidResponse
	}
	return resp, nil
}

func (c *Client) PutBlob(ctx context.Context, hash [32]byte, content []byte) error {
	if int64(len(content)) > clientsync.MaxBlobBytes || sha256.Sum256(content) != hash {
		return errors.New("invalid blob content")
	}
	resp, err := c.request(ctx, http.MethodPut, "/v1/blobs/"+hex.EncodeToString(hash[:]), content, "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classify(resp)
	}
	var out struct {
		Hash string `json:"hash"`
		Size int    `json:"size"`
	}
	if err := decodeJSON(resp, &out, "hash", "size"); err != nil || out.Hash != hex.EncodeToString(hash[:]) || out.Size != len(content) {
		return ErrInvalidResponse
	}
	return nil
}

func (c *Client) ResolveBlob(ctx context.Context, hash [32]byte) ([]byte, error) {
	resp, err := c.request(ctx, http.MethodGet, "/v1/blobs/"+hex.EncodeToString(hash[:]), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := classify(resp)
		var rejected *RejectedError
		if errors.As(err, &rejected) && rejected.Code == "blob_not_found" {
			return nil, clientsync.ErrBlobMissing
		}
		return nil, err
	}
	values := resp.Header.Values("Content-Type")
	if len(values) != 1 {
		return nil, ErrInvalidResponse
	}
	media, params, err := mime.ParseMediaType(values[0])
	if err != nil || media != "application/octet-stream" || len(params) != 0 {
		return nil, ErrInvalidResponse
	}
	if resp.ContentLength < 0 || resp.ContentLength > clientsync.MaxBlobBytes {
		return nil, ErrInvalidResponse
	}
	content, err := io.ReadAll(io.LimitReader(resp.Body, clientsync.MaxBlobBytes+1))
	if err != nil || int64(len(content)) > clientsync.MaxBlobBytes || int64(len(content)) != resp.ContentLength {
		return nil, ErrInvalidResponse
	}
	if sha256.Sum256(content) != hash {
		return nil, clientsync.ErrBlobHashMismatch
	}
	return content, nil
}

func (c *Client) Submit(ctx context.Context, m clientsync.Mutation) (clientsync.Result, error) {
	if err := clientsync.ValidateMutation(m); err != nil {
		return clientsync.Result{}, errors.New("invalid sync mutation")
	}
	type request struct {
		OperationID  string  `json:"operation_id"`
		Mutation     string  `json:"mutation"`
		ObjectID     string  `json:"object_id"`
		ObjectType   string  `json:"object_type"`
		BaseRevision uint64  `json:"base_revision"`
		ParentID     *string `json:"parent_id"`
		Name         string  `json:"name"`
		BlobHash     *string `json:"blob_hash"`
	}
	r := request{OperationID: m.OperationID.String(), Mutation: string(m.Kind), ObjectID: m.ObjectID.String(), ObjectType: string(m.ObjectType), BaseRevision: m.BaseRevision, Name: m.Name}
	if m.ParentID != nil {
		x := m.ParentID.String()
		r.ParentID = &x
	}
	if len(m.BlobHash) != 0 {
		x := hex.EncodeToString(m.BlobHash)
		r.BlobHash = &x
	}
	body, _ := json.Marshal(r)
	resp, err := c.request(ctx, http.MethodPost, "/v1/sync/operations", body, "application/json")
	if err != nil {
		return clientsync.Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return clientsync.Result{}, classify(resp)
	}
	type canonicalResponse struct {
		ObjectType *string         `json:"object_type"`
		Revision   *uint64         `json:"revision"`
		ParentID   json.RawMessage `json:"parent_id"`
		Name       *string         `json:"name"`
		BlobHash   json.RawMessage `json:"blob_hash"`
		Deleted    *bool           `json:"deleted"`
	}
	var out struct {
		Accepted  *bool              `json:"accepted"`
		Conflict  *string            `json:"conflict"`
		Revision  *uint64            `json:"revision"`
		Cursor    *uint64            `json:"cursor"`
		Canonical *canonicalResponse `json:"canonical"`
	}
	if err := decodeJSONNullable(resp, &out, []string{"accepted", "conflict", "revision", "cursor", "canonical"}, []string{"accepted"}); err != nil || out.Accepted == nil {
		return clientsync.Result{}, ErrInvalidResponse
	}
	if *out.Accepted {
		if out.Conflict != nil || out.Canonical != nil || out.Revision == nil || out.Cursor == nil || *out.Revision == 0 || *out.Cursor == 0 || *out.Revision > math.MaxInt64 || *out.Cursor > math.MaxInt64 {
			return clientsync.Result{}, ErrInvalidResponse
		}
		return clientsync.Result{Accepted: true, Revision: *out.Revision, Cursor: *out.Cursor}, nil
	}
	if out.Conflict == nil || !validConflict(*out.Conflict) || out.Revision != nil || out.Cursor != nil {
		return clientsync.Result{}, ErrInvalidResponse
	}
	result := clientsync.Result{Conflict: *out.Conflict}
	if out.Canonical != nil {
		if out.Canonical.ObjectType == nil || out.Canonical.Revision == nil || out.Canonical.Name == nil || out.Canonical.Deleted == nil || out.Canonical.ParentID == nil || out.Canonical.BlobHash == nil {
			return clientsync.Result{}, ErrInvalidResponse
		}
		var parentID, blobHash *string
		if !bytes.Equal(bytes.TrimSpace(out.Canonical.ParentID), []byte("null")) && json.Unmarshal(out.Canonical.ParentID, &parentID) != nil {
			return clientsync.Result{}, ErrInvalidResponse
		}
		if !bytes.Equal(bytes.TrimSpace(out.Canonical.BlobHash), []byte("null")) && json.Unmarshal(out.Canonical.BlobHash, &blobHash) != nil {
			return clientsync.Result{}, ErrInvalidResponse
		}
		canonical, err := validateCanonicalConflict(*out.Canonical.ObjectType, *out.Canonical.Revision, parentID, *out.Canonical.Name, blobHash, *out.Canonical.Deleted)
		if err != nil {
			return clientsync.Result{}, ErrInvalidResponse
		}
		result.Canonical = canonical
	}
	return result, nil
}

func (c *Client) Pull(ctx context.Context, after uint64, limit int) (PullPage, error) {
	if after > math.MaxInt64 || limit <= 0 || limit > 500 {
		return PullPage{}, errors.New("invalid pull bounds")
	}
	resp, err := c.request(ctx, http.MethodGet, "/v1/sync/changes?after="+strconv.FormatUint(after, 10)+"&limit="+strconv.Itoa(limit), nil, "")
	if err != nil {
		return PullPage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return PullPage{}, classify(resp)
	}
	var out struct {
		Changes []struct {
			Cursor      uint64  `json:"cursor"`
			Mutation    string  `json:"mutation"`
			OperationID string  `json:"operation_id"`
			ObjectID    string  `json:"object_id"`
			ObjectType  string  `json:"object_type"`
			Revision    uint64  `json:"revision"`
			ParentID    *string `json:"parent_id"`
			Name        string  `json:"name"`
			BlobHash    *string `json:"blob_hash"`
			Deleted     *bool   `json:"deleted"`
		} `json:"changes"`
		HasMore    *bool   `json:"has_more"`
		NextCursor *uint64 `json:"next_cursor"`
	}
	if err := decodeJSON(resp, &out, "changes", "has_more", "next_cursor"); err != nil || out.Changes == nil || out.HasMore == nil || out.NextCursor == nil {
		return PullPage{}, ErrInvalidResponse
	}
	if *out.NextCursor > math.MaxInt64 || len(out.Changes) > limit {
		return PullPage{}, ErrInvalidResponse
	}
	page := PullPage{HasMore: *out.HasMore, NextCursor: *out.NextCursor, Changes: make([]clientsync.Change, 0, len(out.Changes))}
	for _, x := range out.Changes {
		op, e1 := parseUUID(x.OperationID, true)
		obj, e2 := parseUUID(x.ObjectID, false)
		if e1 != nil || e2 != nil || x.Cursor == 0 || x.Cursor > math.MaxInt64 || x.Revision == 0 || x.Revision > math.MaxInt64 {
			return PullPage{}, ErrInvalidResponse
		}
		kind, objectType := clientsync.MutationKind(x.Mutation), clientsync.ObjectType(x.ObjectType)
		if x.Deleted == nil || (kind != clientsync.Create && kind != clientsync.Update && kind != clientsync.Move && kind != clientsync.Delete) || (objectType != clientsync.Note && objectType != clientsync.Folder) || *x.Deleted != (kind == clientsync.Delete) {
			return PullPage{}, ErrInvalidResponse
		}
		ch := clientsync.Change{Cursor: x.Cursor, Mutation: kind, OperationID: op, ObjectID: obj, ObjectType: objectType, Revision: x.Revision, Name: x.Name, Deleted: *x.Deleted}
		if x.ParentID != nil {
			p, e := parseUUID(*x.ParentID, false)
			if e != nil {
				return PullPage{}, ErrInvalidResponse
			}
			ch.ParentID = &p
		}
		if x.BlobHash != nil {
			b, e := hex.DecodeString(*x.BlobHash)
			if e != nil || len(b) != 32 || *x.BlobHash != hex.EncodeToString(b) {
				return PullPage{}, ErrInvalidResponse
			}
			ch.BlobHash = b
		}
		page.Changes = append(page.Changes, ch)
	}
	return page, nil
}

func parseUUID(raw string, v7 bool) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil || id.String() != raw || id.Variant() != uuid.RFC4122 || (v7 && id.Version() != 7) {
		return uuid.Nil, ErrInvalidResponse
	}
	return id, nil
}

func decodeJSON(resp *http.Response, target any, required ...string) error {
	return decodeJSONNullable(resp, target, required, required)
}

func decodeJSONNullable(resp *http.Response, target any, required, nonNull []string) error {
	if !validJSONContentType(resp.Header.Values("Content-Type")) {
		return ErrInvalidResponse
	}
	content, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes+1))
	if err != nil || len(content) > maxJSONBytes || rejectDuplicates(content) != nil {
		return ErrInvalidResponse
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(content, &fields) != nil || fields == nil {
		return ErrInvalidResponse
	}
	for _, name := range required {
		if _, ok := fields[name]; !ok {
			return ErrInvalidResponse
		}
	}
	for _, name := range nonNull {
		if bytes.Equal(bytes.TrimSpace(fields[name]), []byte("null")) {
			return ErrInvalidResponse
		}
	}
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return ErrInvalidResponse
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrInvalidResponse
	}
	return nil
}
func validJSONContentType(values []string) bool {
	if len(values) != 1 {
		return false
	}
	media, params, err := mime.ParseMediaType(values[0])
	return err == nil && media == "application/json" && (len(params) == 0 || (len(params) == 1 && strings.EqualFold(params["charset"], "utf-8")))
}

func rejectDuplicates(content []byte) error {
	d := json.NewDecoder(bytes.NewReader(content))
	if err := scan(d); err != nil {
		return err
	}
	_, err := d.Token()
	if !errors.Is(err, io.EOF) {
		return ErrInvalidResponse
	}
	return nil
}
func scan(d *json.Decoder) error {
	t, e := d.Token()
	if e != nil {
		return e
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	if delim == '{' {
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return e
			}
			s, ok := k.(string)
			if !ok || seen[s] {
				return ErrInvalidResponse
			}
			seen[s] = true
			if e := scan(d); e != nil {
				return e
			}
		}
		_, e = d.Token()
		return e
	}
	if delim == '[' {
		for d.More() {
			if e := scan(d); e != nil {
				return e
			}
		}
		_, e = d.Token()
		return e
	}
	return ErrInvalidResponse
}

func validateCanonicalConflict(objectType string, revision uint64, parentRaw *string, name string, hashRaw *string, deleted bool) (*clientsync.CanonicalState, error) {
	if revision == 0 || revision > math.MaxInt64 || naming.ValidateComponent(name) != nil {
		return nil, ErrInvalidResponse
	}
	state := &clientsync.CanonicalState{ObjectType: clientsync.ObjectType(objectType), Revision: revision, Name: name, Deleted: deleted}
	if state.ObjectType != clientsync.Note && state.ObjectType != clientsync.Folder {
		return nil, ErrInvalidResponse
	}
	if parentRaw != nil {
		id, err := uuid.Parse(*parentRaw)
		if err != nil || id == uuid.Nil || id.Variant() != uuid.RFC4122 || id.String() != *parentRaw {
			return nil, ErrInvalidResponse
		}
		state.ParentID = &id
	}
	if hashRaw != nil {
		hash, err := hex.DecodeString(*hashRaw)
		if err != nil || len(hash) != sha256.Size || hex.EncodeToString(hash) != *hashRaw {
			return nil, ErrInvalidResponse
		}
		state.BlobHash = hash
	}
	if state.ObjectType == clientsync.Note && len(state.BlobHash) != sha256.Size || state.ObjectType == clientsync.Folder && len(state.BlobHash) != 0 {
		return nil, ErrInvalidResponse
	}
	return state, nil
}

func validConflict(code string) bool {
	switch code {
	case "object_exists", "object_missing", "object_deleted", "base_revision_mismatch", "parent_unavailable", "path_collision", "folder_not_empty", "folder_cycle", "type_mismatch":
		return true
	default:
		return false
	}
}

func classify(resp *http.Response) error {
	content, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes+1))
	if err != nil || len(content) > maxJSONBytes || rejectDuplicates(content) != nil || !validJSONContentType(resp.Header.Values("Content-Type")) {
		return ErrInvalidResponse
	}
	var out struct {
		Error string `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&out) != nil || out.Error == "" {
		return ErrInvalidResponse
	}
	if !validAPIError(resp.StatusCode, out.Error) {
		return ErrInvalidResponse
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrReauthRequired
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return ErrRetryable
	}
	if resp.StatusCode == http.StatusConflict && out.Error == "operation_replay_mismatch" {
		return ErrReplayMismatch
	}
	return &RejectedError{Status: resp.StatusCode, Code: out.Error}
}

func validAPIError(status int, code string) bool {
	switch status {
	case http.StatusBadRequest:
		return code == "invalid_request"
	case http.StatusUnauthorized:
		return code == "invalid_session"
	case http.StatusNotFound:
		return code == "blob_not_found"
	case http.StatusConflict:
		return code == "blob_unavailable" || code == "operation_replay_mismatch" || code == "preserve_delete_unavailable"
	case http.StatusRequestEntityTooLarge:
		return code == "quota_exceeded" || code == "blob_too_large"
	case http.StatusUnprocessableEntity:
		return code == "hash_mismatch"
	case http.StatusTooManyRequests:
		return code == "rate_limited"
	case http.StatusInternalServerError:
		return code == "internal_error"
	default:
		return false
	}
}
