package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

const TaskTokenTTL = 15 * time.Minute

type Client struct {
	httpClient *http.Client
	baseURL    string
	tokens     *TaskTokenSigner
}

func NewClient(baseURL string, tokens *TaskTokenSigner) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 10 * time.Minute},
		baseURL:    baseURL,
		tokens:     tokens,
	}
}

// Dispatch sends task as a real HTTP POST to targetAgentID's own
// tasks/send endpoint — a genuine loopback network call, not an in-process
// function call, so a specialist agent is dispatched to exactly the way
// an external A2A-speaking caller eventually could be. taskID is minted by
// the caller (graph's delegate node) *before* calling Dispatch specifically
// so it can register EventBus.LinkChild first and not miss the
// specialist's early events — see envelope.go's TaskSendParams.ID doc.
func (c *Client) Dispatch(ctx context.Context, orgID, dispatchingRunID, targetAgentID, taskID uuid.UUID, task string) (Task, error) {
	if c.tokens.secret.IsEmpty() {
		return Task{}, fmt.Errorf("a2a: dispatch: %w: A2A_TASK_TOKEN_SECRET is not configured", ErrTaskTokenInvalid)
	}
	token := c.tokens.Sign(orgID, targetAgentID, time.Now().Add(TaskTokenTTL))

	reqBody := TaskSendRequest{
		JSONRPC: "2.0",
		ID:      uuid.NewString(),
		Method:  "tasks/send",
		Params: TaskSendParams{
			ID:        taskID.String(),
			SessionID: dispatchingRunID.String(),
			Message:   TextMessage("user", task),
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return Task{}, fmt.Errorf("a2a: marshal tasks/send request: %w", err)
	}

	url := fmt.Sprintf("%s/api/v1/a2a/agents/%s/tasks/send", c.baseURL, targetAgentID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Task{}, fmt.Errorf("a2a: build tasks/send request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return Task{}, fmt.Errorf("a2a: tasks/send call to agent %s: %w", targetAgentID, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Task{}, fmt.Errorf("a2a: read tasks/send response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Task{}, fmt.Errorf("a2a: tasks/send to agent %s returned %d: %s", targetAgentID, resp.StatusCode, string(respBody))
	}

	var rpcResp TaskSendResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return Task{}, fmt.Errorf("a2a: unmarshal tasks/send response: %w", err)
	}
	if rpcResp.Error != nil {
		return Task{}, fmt.Errorf("a2a: agent %s returned error %d: %s", targetAgentID, rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if rpcResp.Result == nil {
		return Task{}, fmt.Errorf("a2a: agent %s returned no result", targetAgentID)
	}
	if rpcResp.Result.Status.State != "completed" {
		return *rpcResp.Result, fmt.Errorf("a2a: agent %s task ended in state %q", targetAgentID, rpcResp.Result.Status.State)
	}
	return *rpcResp.Result, nil
}
