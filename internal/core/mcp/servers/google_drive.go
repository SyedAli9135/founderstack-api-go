package servers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	mcp "github.com/founderstack/api/internal/core/mcp"
)

// driveAPIBase / driveUploadBase are vars so google_drive_test.go can
// point them at a fake server.
var (
	driveAPIBase    = "https://www.googleapis.com/drive/v3"
	driveUploadBase = "https://www.googleapis.com/upload/drive/v3"
)

// Scoped to drive.file (avoids Google's paid CASA assessment), so
// list_files/read_file only ever see files this app created or the
// founder explicitly shared with it — a fresh org sees an empty list
// until create_file has made something, which is the scope working as
// intended.
func NewGoogleDriveServer() *gomcp.Server {
	server := gomcp.NewServer(&gomcp.Implementation{Name: "google_drive", Version: "1.0.0"}, nil)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "list_files",
		Description: "List Drive files this integration can see (drive.file scope: only files this app created or the founder explicitly shared with it).",
		Annotations: mcp.ReadOnly(),
	}, driveListFiles)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "read_file",
		Description: "Read a Drive file's raw content. Plain text/binary files only — Google-native Docs/Sheets/Slides need the export API, not implemented here.",
		Annotations: mcp.ReadOnly(),
	}, driveReadFile)

	gomcp.AddTool(server, &gomcp.Tool{
		Name:        "create_file",
		Description: "Create a new file in Drive with the given text content.",
		Annotations: mcp.ReversibleWrite(),
	}, driveCreateFile)

	return server
}

type driveListFilesInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"Maximum files to return (default 20, max 100)"`
}

type driveFileSummary struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MimeType     string `json:"mime_type"`
	ModifiedTime string `json:"modified_time,omitempty"`
}

type driveListFilesOutput struct {
	Files []driveFileSummary `json:"files"`
}

type driveFilesListResponse struct {
	Files []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		MimeType     string `json:"mimeType"`
		ModifiedTime string `json:"modifiedTime"`
	} `json:"files"`
}

func driveListFiles(ctx context.Context, req *gomcp.CallToolRequest, in driveListFilesInput) (*gomcp.CallToolResult, driveListFilesOutput, error) {
	token, ok := mcp.TokenFromRequest(req)
	if !ok {
		return nil, driveListFilesOutput{}, fmt.Errorf("google_drive: no token in request metadata")
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	endpoint := fmt.Sprintf("%s/files?pageSize=%d&fields=files(id,name,mimeType,modifiedTime)", driveAPIBase, limit)
	var resp driveFilesListResponse
	if err := doJSON(ctx, "GET", endpoint, token, nil, &resp); err != nil {
		return nil, driveListFilesOutput{}, fmt.Errorf("google_drive: list files: %w", err)
	}

	out := driveListFilesOutput{Files: make([]driveFileSummary, 0, len(resp.Files))}
	for _, f := range resp.Files {
		out.Files = append(out.Files, driveFileSummary{ID: f.ID, Name: f.Name, MimeType: f.MimeType, ModifiedTime: f.ModifiedTime})
	}
	return nil, out, nil
}

type driveReadFileInput struct {
	FileID string `json:"file_id" jsonschema:"Google Drive file ID"`
}

type driveReadFileOutput struct {
	Content string `json:"content"`
}

func driveReadFile(ctx context.Context, req *gomcp.CallToolRequest, in driveReadFileInput) (*gomcp.CallToolResult, driveReadFileOutput, error) {
	token, ok := mcp.TokenFromRequest(req)
	if !ok {
		return nil, driveReadFileOutput{}, fmt.Errorf("google_drive: no token in request metadata")
	}
	if in.FileID == "" {
		return nil, driveReadFileOutput{}, fmt.Errorf("google_drive: file_id is required")
	}

	// alt=media returns raw bytes, not JSON, so this can't go through
	// doAndDecode. Always a GET, so unlike doAndDecode's isWrite guard,
	// a network-level failure is safe to retry here too.
	endpoint := fmt.Sprintf("%s/files/%s?alt=media", driveAPIBase, in.FileID)

	var lastErr error
	for attempt := 1; attempt <= toolCallMaxAttempts; attempt++ {
		httpReq, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
		if err != nil {
			return nil, driveReadFileOutput{}, fmt.Errorf("servers: build request: %w", err)
		}
		httpReq.Header.Set("Authorization", "Bearer "+token)

		resp, err := httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("%w: %s: %w", mcp.ErrToolRetryable, endpoint, err)
			if attempt < toolCallMaxAttempts && sleepBackoff(ctx, attempt) {
				continue
			}
			return nil, driveReadFileOutput{}, lastErr
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5 MiB cap
		resp.Body.Close()
		if readErr != nil {
			return nil, driveReadFileOutput{}, fmt.Errorf("google_drive: read response: %w", readErr)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			classified := mcp.ClassifyToolHTTPError(resp.StatusCode, fmt.Sprintf("%s returned: %s", endpoint, truncate(body, 500)))
			if errors.Is(classified, mcp.ErrToolRetryable) && attempt < toolCallMaxAttempts && sleepBackoff(ctx, attempt) {
				lastErr = classified
				continue
			}
			return nil, driveReadFileOutput{}, classified
		}

		return nil, driveReadFileOutput{Content: string(body)}, nil
	}
	return nil, driveReadFileOutput{}, lastErr
}

type driveCreateFileInput struct {
	Name     string `json:"name" jsonschema:"File name"`
	Content  string `json:"content" jsonschema:"File content"`
	MimeType string `json:"mime_type,omitempty" jsonschema:"MIME type (default text/plain)"`
}

type driveCreateFileOutput struct {
	FileID string `json:"file_id"`
}

type driveCreatedFileResponse struct {
	ID string `json:"id"`
}

func driveCreateFile(ctx context.Context, req *gomcp.CallToolRequest, in driveCreateFileInput) (*gomcp.CallToolResult, driveCreateFileOutput, error) {
	token, ok := mcp.TokenFromRequest(req)
	if !ok {
		return nil, driveCreateFileOutput{}, fmt.Errorf("google_drive: no token in request metadata")
	}
	if in.Name == "" {
		return nil, driveCreateFileOutput{}, fmt.Errorf("google_drive: name is required")
	}
	mimeType := in.MimeType
	if mimeType == "" {
		mimeType = "text/plain"
	}

	body, contentType, err := driveMultipartBody(in.Name, mimeType, in.Content)
	if err != nil {
		return nil, driveCreateFileOutput{}, fmt.Errorf("google_drive: build upload body: %w", err)
	}

	endpoint := driveUploadBase + "/files?uploadType=multipart&fields=id"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, body)
	if err != nil {
		return nil, driveCreateFileOutput{}, fmt.Errorf("servers: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", contentType)

	var created driveCreatedFileResponse
	if err := doAndDecode(httpReq, &created); err != nil {
		return nil, driveCreateFileOutput{}, fmt.Errorf("google_drive: create file: %w", err)
	}

	return nil, driveCreateFileOutput{FileID: created.ID}, nil
}

// Builds a multipart/related body (metadata part + content part), the
// shape Google's upload API expects — not multipart/form-data.
func driveMultipartBody(name, mimeType, content string) (*bytes.Buffer, string, error) {
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)

	metaPart, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	if err != nil {
		return nil, "", err
	}
	if err := json.NewEncoder(metaPart).Encode(map[string]string{"name": name}); err != nil {
		return nil, "", err
	}

	contentPart, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": {mimeType}})
	if err != nil {
		return nil, "", err
	}
	if _, err := contentPart.Write([]byte(content)); err != nil {
		return nil, "", err
	}

	if err := w.Close(); err != nil {
		return nil, "", err
	}
	// FormDataContentType() would say multipart/form-data; Google needs
	// multipart/related with the same boundary, built by hand.
	return buf, "multipart/related; boundary=" + w.Boundary(), nil
}
