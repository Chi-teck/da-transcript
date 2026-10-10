package app

import (
	"bytes"
	"cmp"
	"encoding/json"
)

// Typed views of the JSON the store keeps in session_items.message_json. Only
// what the renderer reads is declared; everything else passes through. Fields
// the store is loose about are flexString or json.RawMessage so one odd value
// does not take a whole row down.
//
// This is the shape of what the store hands over, not a rendering decision, so
// it sits beside the store rather than inside the renderer: how a tool item is
// drawn changes often and what a tool item contains does not.
//
// Base64 and provider payloads (inline_data, image_url.url, thinking and
// thought signatures, openai_response, provider_state), tool_definitions and
// compaction are not declared, so the decoder skips them.

type itemData struct {
	Role             flexString      `json:"role"`              // message
	Content          flexString      `json:"content"`           // message
	MultiContent     []messagePart   `json:"multi_content"`     // user, tool
	ReasoningContent flexString      `json:"reasoning_content"` // assistant
	ToolCalls        []toolCall      `json:"tool_calls"`        // assistant
	ToolCallID       flexString      `json:"tool_call_id"`      // tool
	IsError          json.RawMessage `json:"is_error"`          // tool; truthy() reads it
	MessageID        flexString      `json:"message_id"`        // user, assistant
	FinishReason     flexString      `json:"finish_reason"`     // assistant; absent = cut off or streaming
	Model            flexString      `json:"model"`             // assistant, evaluation: "provider/model"
	Usage            *stepUsage      `json:"usage"`             // assistant, evaluation; a summary's usage_json is put here
	Cost             json.RawMessage `json:"cost"`              // assistant, evaluation
	CreatedAt        flexString      `json:"created_at"`        // all but subsession and summary
	Message          flexString      `json:"message"`           // error (and termination)
	Code             flexString      `json:"code"`              // error
	AgentName        flexString      `json:"agent_name"`        // error, evaluation: their agent_name column is NULL
	Reason           flexString      `json:"reason"`            // termination
}

// messagePart is one multi_content entry: text, document, the legacy
// image_url and file, or a type kept by name. image_url.url is a data: URL and
// is not declared, so it is never decoded.
type messagePart struct {
	Type     flexString    `json:"type"`
	Text     flexString    `json:"text"`
	Document *partDocument `json:"document"`
	File     *partFile     `json:"file"`
}

// partDocument is an attachment. Its bytes sit in source.inline_data as
// base64; that is not declared either.
type partDocument struct {
	Name     flexString `json:"name"`
	MimeType flexString `json:"mime_type"`
}

// partFile is an attachment as docker-agent wrote it before v1.55, by path,
// which a store upgraded since still holds. Images of that age are image_url
// parts instead, with no name at all.
type partFile struct {
	Path     flexString `json:"path"`
	MimeType flexString `json:"mime_type"`
}

type toolCall struct {
	ID       flexString `json:"id"`
	Function struct {
		Name      flexString    `json:"name"`
		Arguments toolArguments `json:"arguments"`
	} `json:"function"`
}

// waitResult is what wait_background_agents returns as its result's content
// (pkg/tools/builtin/agent/wait.go:waitResult): an error when the wait itself
// was cancelled, and one entry per task it was asked about. A task's output, or
// the error it failed with, is cut at 4 KB, and truncated says so.
type waitResult struct {
	Error flexString `json:"error"`
	Tasks []waitTask `json:"tasks"`
}

type waitTask struct {
	TaskID    flexString      `json:"task_id"`
	Status    flexString      `json:"status"` // completed, failed, stopped, running, not_found
	Output    flexString      `json:"output"`
	Error     flexString      `json:"error"`
	Truncated json.RawMessage `json:"truncated"` // truthy() reads it
}

type stepUsage struct {
	Input      json.RawMessage `json:"input_tokens"`
	Output     json.RawMessage `json:"output_tokens"`
	CacheRead  json.RawMessage `json:"cached_input_tokens"`
	CacheWrite json.RawMessage `json:"cached_write_tokens"`
}

// toolArguments is a call's function.arguments: a JSON string holding the
// JSON the model wrote, decoded a second time here. JSON is kept raw and not
// re-encoded, so the model's key order survives into the in: block. Anything
// else — malformed arguments, plain text — is kept as a JSON string of that
// text, so it stays printable, and "" is nil. A bare JSON string is kept the
// same way, its quotes and escapes part of the text: kept raw, it would print
// as the text inside, like arguments that were not JSON. An arguments value
// that is already an object comes through flexString as its compact JSON. It
// never fails the row.
type toolArguments json.RawMessage

func (a *toolArguments) UnmarshalJSON(data []byte) error {
	var s flexString
	_ = s.UnmarshalJSON(data) // never fails
	switch {
	case s == "":
		*a = nil
	case json.Valid([]byte(s)) && bytes.TrimSpace([]byte(s))[0] != '"':
		*a = toolArguments(s)
	default:
		// Not json.Marshal: it would write a shell command's && and > as
		// \u0026\u0026 and \u003e.
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(string(s)) // a string always encodes
		*a = bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	}
	return nil
}

// decodeItem reads one session_items row into r. It never fails: whatever this
// version cannot read, it names in r.unknown, and that item adds no tokens.
// JSON that does not decode leaves r.data zero, so the item takes the time of
// the one before it.
func decodeItem(r *itemRow, message, usage []byte) {
	// read decodes message_json into r.data; NULL leaves it zero.
	read := func() bool {
		if message == nil {
			return true
		}
		if err := json.Unmarshal(message, &r.data); err != nil {
			r.data = itemData{}
			r.unknown = r.typ + " (unreadable)"
			return false
		}
		return true
	}
	switch r.typ {
	case "message":
		if !read() {
			return
		}
		switch r.data.Role {
		case "user", "system", "assistant", "tool":
		case "":
			r.unknown = "message (no role)"
		default:
			r.unknown = "message (role " + string(r.data.Role) + ")"
		}
	case "error", "evaluation", "termination":
		read()
	case "summary":
		// message_json, which a provider-native compaction fills with the
		// provider's result, is not read; the summarising call's usage is.
		if len(usage) > 0 {
			if err := json.Unmarshal(usage, &r.data.Usage); err != nil {
				r.data = itemData{}
				r.unknown = "summary (unreadable)"
			}
		}
	case "subsession":
		// message_json is NULL: the row only points at the child.
	default:
		// Decoded only for its created_at; a usage it carries is not trusted.
		if read() {
			r.unknown = cmp.Or(r.typ, "item (no type)")
		}
	}
}

// errorText renders an error item, stored as {message, code}, as
// "<code>: <message>", or whichever of the two it has. One with neither is
// "?": an error is never drawn as a bare label.
func errorText(code, message string) string {
	switch {
	case code != "" && message != "":
		return code + ": " + message
	case code != "" || message != "":
		return code + message
	}
	return "?"
}

// numberText is what an f-string prints for a JSON number: the literal for an
// int, the same for a float (whose repr is its shortest round-trip form, which
// is how the store spelled it). Anything that is not a number prints as dflt.
func numberText(raw json.RawMessage, dflt string) string {
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil || n == "" {
		return dflt
	}
	return string(n)
}

func numberValue(raw json.RawMessage) (float64, bool) {
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil || n == "" {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}
