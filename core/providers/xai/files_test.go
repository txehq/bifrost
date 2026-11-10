package xai

import (
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// Upload sends the JSONL as the multipart file part, xAI's only documented field, with the
// key's bearer token, and returns the file id a batch's input_file_id names (TXE-6664).
func TestFileUploadSendsOnlyTheFilePart(t *testing.T) {
	var gotParts map[string]string
	var gotFilename, gotAuth, gotPath string
	stub := &batchStub{}
	stub.handler = func(w http.ResponseWriter, r *http.Request, body string) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("content type: %v", err)
		}
		reader := multipart.NewReader(strings.NewReader(body), params["boundary"])
		gotParts = map[string]string{}
		for {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(part)
			gotParts[part.FormName()] = string(data)
			if part.FormName() == "file" {
				gotFilename = part.FileName()
			}
		}
		_, _ = io.WriteString(w, `{"id":"file-abc123","object":"file","bytes":96,"created_at":1794283200,"filename":"batch_requests.jsonl"}`)
	}
	provider := newBatchTestProvider(stub.serve(t).URL)
	jsonl := []byte(`{"custom_id":"chat-1","method":"POST","url":"/v1/chat/completions","body":{"model":"grok-4.3","messages":[]}}` + "\n")
	resp, bErr := provider.FileUpload(batchTestCtx(), batchKey("k", "sk-xai"), &schemas.BifrostFileUploadRequest{
		Provider: schemas.XAI, File: jsonl, Filename: "batch_requests.jsonl", Purpose: schemas.FilePurposeBatch,
	})
	if bErr != nil {
		t.Fatalf("FileUpload: %+v", bErr.Error)
	}
	if gotPath != "/v1/files" || gotAuth != "Bearer sk-xai" {
		t.Fatalf("path=%s auth=%s", gotPath, gotAuth)
	}
	if len(gotParts) != 1 || gotParts["file"] != string(jsonl) || gotFilename != "batch_requests.jsonl" {
		t.Fatalf("parts = %v (filename %q), want only the file part", gotParts, gotFilename)
	}
	if resp.ID != "file-abc123" || resp.Bytes != 96 || resp.CreatedAt != 1794283200 || resp.Purpose != schemas.FilePurposeBatch {
		t.Fatalf("upload response = %+v", resp)
	}
}

// Retrieve, list, delete and content reach xAI's file routes and map the alternate field
// names (file_id, size_bytes, files, pagination_token).
func TestFileRoutes(t *testing.T) {
	stub := &batchStub{}
	stub.handler = func(w http.ResponseWriter, r *http.Request, _ string) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/files":
			_, _ = io.WriteString(w, `{"files":[{"file_id":"file-1","size_bytes":10,"filename":"a.jsonl","created_at":"2026-11-10T04:00:00Z"}],"pagination_token":"next"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/files/file-1":
			_, _ = io.WriteString(w, `{"id":"file-1","bytes":10,"filename":"a.jsonl","purpose":"batch"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/files/file-1/content":
			w.Header().Set("Content-Type", "application/jsonl")
			_, _ = io.WriteString(w, "{\"custom_id\":\"a\"}\n")
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/files/file-1":
			_, _ = io.WriteString(w, `{"id":"file-1","deleted":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"code":"not_found","error":"no such file"}`)
		}
	}
	provider := newBatchTestProvider(stub.serve(t).URL)
	ctx := batchTestCtx()
	keys := []schemas.Key{batchKey("k", "sk")}

	list, bErr := provider.FileList(ctx, keys, &schemas.BifrostFileListRequest{Limit: 5})
	if bErr != nil {
		t.Fatalf("FileList: %+v", bErr.Error)
	}
	if len(list.Data) != 1 || list.Data[0].ID != "file-1" || list.Data[0].Bytes != 10 || list.Data[0].CreatedAt != 1794283200 || !list.HasMore || list.After == nil {
		t.Fatalf("list = %+v", list)
	}
	if calls := stub.recorded(); calls[0].Query != "limit=5" {
		t.Fatalf("list query = %q", calls[0].Query)
	}

	got, bErr := provider.FileRetrieve(ctx, keys, &schemas.BifrostFileRetrieveRequest{FileID: "file-1"})
	if bErr != nil || got.ID != "file-1" || got.Purpose != schemas.FilePurposeBatch {
		t.Fatalf("FileRetrieve = %+v, %+v", got, bErr)
	}

	content, bErr := provider.FileContent(ctx, keys, &schemas.BifrostFileContentRequest{FileID: "file-1"})
	if bErr != nil || string(content.Content) != "{\"custom_id\":\"a\"}\n" || content.ContentType != "application/jsonl" {
		t.Fatalf("FileContent = %+v, %+v", content, bErr)
	}

	deleted, bErr := provider.FileDelete(ctx, keys, &schemas.BifrostFileDeleteRequest{FileID: "file-1"})
	if bErr != nil || !deleted.Deleted || deleted.ID != "file-1" {
		t.Fatalf("FileDelete = %+v, %+v", deleted, bErr)
	}

	_, bErr = provider.FileRetrieve(ctx, keys, &schemas.BifrostFileRetrieveRequest{FileID: "file-missing"})
	if bErr == nil || bErr.StatusCode == nil || *bErr.StatusCode != http.StatusNotFound || bErr.Error.Message != "no such file" {
		t.Fatalf("missing file error = %+v", bErr)
	}

	_, bErr = provider.FileRetrieve(ctx, keys, &schemas.BifrostFileRetrieveRequest{FileID: "../batches"})
	if bErr == nil || bErr.StatusCode == nil || *bErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("path-shaping id = %+v, want 400 before dispatch", bErr)
	}
}
