package a2a

// JSON-RPC 2.0 envelope for the one method this codebase implements,
// "tasks/send" — see card.go's package doc for scope.

type TaskSendRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      string         `json:"id"`
	Method  string         `json:"method"`
	Params  TaskSendParams `json:"params"`
}

type TaskSendParams struct {
	ID        string  `json:"id"`
	SessionID string  `json:"sessionId"`
	Message   Message `json:"message"`
}

type Message struct {
	Role  string `json:"role"`
	Parts []Part `json:"parts"`
}

type Part struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type TaskSendResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      string        `json:"id"`
	Result  *Task         `json:"result,omitempty"`
	Error   *JSONRPCError `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type Task struct {
	ID        string     `json:"id"`
	Status    TaskStatus `json:"status"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

type TaskStatus struct {
	// State is "completed" or "failed" — this codebase's tasks/send is
	// synchronous (see Capabilities.Streaming's doc comment), so no
	// "working"/"input-required" intermediate state is ever returned.
	State string `json:"state"`
}

type Artifact struct {
	Name  string `json:"name"`
	Parts []Part `json:"parts"`
}

// TextMessage builds a single-part text Message — every request/response
// this codebase sends is plain text, never multi-part/binary.
func TextMessage(role, text string) Message {
	return Message{Role: role, Parts: []Part{{Type: "text", Text: text}}}
}

// Text returns t's first text part, "" if it has none — the inverse of
// TextMessage for reading a Task's Artifacts back out.
func (t Task) Text() string {
	for _, art := range t.Artifacts {
		for _, p := range art.Parts {
			if p.Type == "text" {
				return p.Text
			}
		}
	}
	return ""
}
