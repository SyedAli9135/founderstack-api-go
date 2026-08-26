package servers

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	mcp "github.com/founderstack/api/internal/core/mcp"
)

func connectGoogleDriveServer(t *testing.T) *gomcp.ClientSession {
	t.Helper()
	server := NewGoogleDriveServer()
	serverTransport, clientTransport := gomcp.NewInMemoryTransports()

	ctx := context.Background()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("connect server: %v", err)
	}
	client := gomcp.NewClient(&gomcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func swapDriveAPIBases(apiURL, uploadURL string) func() {
	origAPI, origUpload := driveAPIBase, driveUploadBase
	driveAPIBase, driveUploadBase = apiURL, uploadURL
	return func() { driveAPIBase, driveUploadBase = origAPI, origUpload }
}

func TestGoogleDrive_ListFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"files":[{"id":"f1","name":"report.txt","mimeType":"text/plain","modifiedTime":"2026-01-01T00:00:00Z"}]}`))
	}))
	defer srv.Close()
	defer swapDriveAPIBases(srv.URL, srv.URL)()

	session := connectGoogleDriveServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "list_files",
		Meta: mcp.WithToken("drive-test-token"),
	})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}

	var out driveListFilesOutput
	if err := unmarshalStructured(result, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Files) != 1 || out.Files[0].Name != "report.txt" {
		t.Fatalf("files = %+v, want one report.txt", out.Files)
	}
}

func TestGoogleDrive_ReadFile(t *testing.T) {
	var gotPath, gotAlt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAlt = r.URL.Query().Get("alt")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello from drive"))
	}))
	defer srv.Close()
	defer swapDriveAPIBases(srv.URL, srv.URL)()

	session := connectGoogleDriveServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{"file_id": "f1"},
		Meta:      mcp.WithToken("drive-test-token"),
	})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}
	if gotPath != "/files/f1" || gotAlt != "media" {
		t.Errorf("path=%q alt=%q, want /files/f1 alt=media", gotPath, gotAlt)
	}

	var out driveReadFileOutput
	if err := unmarshalStructured(result, &out); err != nil {
		t.Fatal(err)
	}
	if out.Content != "hello from drive" {
		t.Fatalf("content = %q, want %q", out.Content, "hello from drive")
	}
}

func TestGoogleDrive_ReadFile_RetriesOnServerErrorThenSucceeds(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("recovered content"))
	}))
	defer srv.Close()
	defer swapDriveAPIBases(srv.URL, srv.URL)()

	session := connectGoogleDriveServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{"file_id": "f1"},
		Meta:      mcp.WithToken("drive-test-token"),
	})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}
	if calls != 2 {
		t.Fatalf("server received %d requests, want 2 (1 retry after the 500)", calls)
	}

	var out driveReadFileOutput
	if err := unmarshalStructured(result, &out); err != nil {
		t.Fatal(err)
	}
	if out.Content != "recovered content" {
		t.Fatalf("content = %q, want %q", out.Content, "recovered content")
	}
}

func TestGoogleDrive_ReadFile_DoesNotRetryTerminalStatus(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	defer swapDriveAPIBases(srv.URL, srv.URL)()

	session := connectGoogleDriveServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{"file_id": "does-not-exist"},
		Meta:      mcp.WithToken("drive-test-token"),
	})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if !result.IsError {
		t.Fatal("result.IsError = false, want true for a 404")
	}
	if calls != 1 {
		t.Fatalf("server received %d requests, want exactly 1 (a 404 must not retry)", calls)
	}
}

func TestGoogleDrive_CreateFile(t *testing.T) {
	var gotContentType string
	var gotMetaName, gotContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		_, params, err := mime.ParseMediaType(gotContentType)
		if err != nil {
			t.Fatalf("parse content-type: %v", err)
		}
		if !strings.HasPrefix(gotContentType, "multipart/related") {
			t.Errorf("Content-Type = %q, want multipart/related prefix", gotContentType)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		metaPart, err := mr.NextPart()
		if err != nil {
			t.Fatalf("read meta part: %v", err)
		}
		metaBytes, _ := io.ReadAll(metaPart)
		gotMetaName = string(metaBytes)

		contentPart, err := mr.NextPart()
		if err != nil {
			t.Fatalf("read content part: %v", err)
		}
		contentBytes, _ := io.ReadAll(contentPart)
		gotContent = string(contentBytes)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"newfile1"}`))
	}))
	defer srv.Close()
	defer swapDriveAPIBases(srv.URL, srv.URL)()

	session := connectGoogleDriveServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "create_file",
		Arguments: map[string]any{"name": "notes.txt", "content": "hello world"},
		Meta:      mcp.WithToken("drive-test-token"),
	})
	if err != nil {
		t.Fatalf("CallTool error = %v", err)
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %+v", result.Content)
	}
	if !strings.Contains(gotMetaName, `"name":"notes.txt"`) {
		t.Errorf("metadata part = %q, want it to contain the file name", gotMetaName)
	}
	if gotContent != "hello world" {
		t.Errorf("content part = %q, want %q", gotContent, "hello world")
	}

	var out driveCreateFileOutput
	if err := unmarshalStructured(result, &out); err != nil {
		t.Fatal(err)
	}
	if out.FileID != "newfile1" {
		t.Fatalf("file_id = %q, want newfile1", out.FileID)
	}
}

func TestGoogleDrive_ReadFile_MissingIDIsToolError(t *testing.T) {
	session := connectGoogleDriveServer(t)
	result, err := session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{},
		Meta:      mcp.WithToken("drive-test-token"),
	})
	if err != nil {
		t.Fatalf("CallTool protocol error = %v, want a tool-level error instead", err)
	}
	if !result.IsError {
		t.Fatal("result.IsError = false, want true for missing file_id")
	}
}
