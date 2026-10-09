package app

import (
	"encoding/json"
	"testing"
)

// decodeItem never fails: what this version cannot read comes back named, as
// the placeholder will name it, and everything else comes back unnamed.
func TestDecodeItem(t *testing.T) {
	for _, c := range []struct {
		name           string
		typ, msg, use  string
		unknown        string
		input, created string // the usage's input_tokens and the created_at read, "" for none
	}{
		{"user", "message", `{"role":"user","content":"hi","created_at":"2026-10-08T09:00:00+05:00"}`, "",
			"", "", "2026-10-08T09:00:00+05:00"},
		{"system", "message", `{"role":"system","content":"You are a member of a team"}`, "", "", "", ""},
		{"assistant", "message", `{"role":"assistant","content":"ok","finish_reason":"stop",
		  "usage":{"input_tokens":67,"output_tokens":87},"tool_calls":[{"id":"c1","type":"function",
		  "function":{"name":"shell","arguments":"{\"cmd\":\"ls\"}"}}]}`, "", "", "67", ""},
		{"tool", "message", `{"role":"tool","tool_call_id":"c1","content":"x","is_error":true,
		  "multi_content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}`, "", "", "", ""},
		{"error", "error", `{"message":"model failed","code":"model_error"}`, "", "", "", ""},
		{"error without JSON", "error", "", "", "", "", ""},
		{"evaluation", "evaluation", `{"id":"e1","evaluator":"judge","model":"p/m",
		  "usage":{"input_tokens":30,"output_tokens":3},"cost":0.001,
		  "created_at":"2026-10-08T10:00:00.123456789+05:00"}`, "", "", "30", "2026-10-08T10:00:00.123456789+05:00"},
		{"termination", "termination", `{"reason":"budget_exceeded","message":"run budget"}`, "", "", "", ""},
		{"summary", "summary", "", `{"input_tokens":400,"output_tokens":100}`, "", "400", ""},
		{"summary without usage", "summary", "", "", "", "", ""},
		{"subsession", "subsession", "", "", "", "", ""},
		{"unknown role", "message", `{"role":"narrator","content":"x"}`, "", "message (role narrator)", "", ""},
		{"no role", "message", `{"content":"x"}`, "", "message (no role)", "", ""},
		{"unknown type", "mystery", `{"created_at":"2026-10-08T09:00:00+05:00"}`, "", "mystery", "", "2026-10-08T09:00:00+05:00"},
		{"no type", "", `{"created_at":"2026-10-08T09:00:00+05:00"}`, "", "item (no type)", "", "2026-10-08T09:00:00+05:00"},
		{"malformed", "message", `{"role":"user",`, "", "message (unreadable)", "", ""},
		{"structural mismatch", "message", `{"role":"assistant","tool_calls":{"id":"c1"},
		  "created_at":"2026-10-08T09:00:00+05:00"}`, "", "message (unreadable)", "", ""},
		{"bad usage", "summary", "", `{"input_tokens":`, "summary (unreadable)", "", ""},
	} {
		var msg, use []byte
		if c.msg != "" {
			msg = []byte(c.msg)
		}
		use = []byte(c.use) // usage_json is NOT NULL: '' rather than NULL
		r := itemRow{typ: c.typ}
		decodeItem(&r, msg, use)
		if r.unknown != c.unknown {
			t.Errorf("%s: unknown = %q, want %q", c.name, r.unknown, c.unknown)
		}
		input := ""
		if r.data.Usage != nil {
			input = string(r.data.Usage.Input)
		}
		if input != c.input {
			t.Errorf("%s: usage input = %q, want %q", c.name, input, c.input)
		}
		if string(r.data.CreatedAt) != c.created {
			t.Errorf("%s: created_at = %q, want %q", c.name, r.data.CreatedAt, c.created)
		}
	}
}

// function.arguments is JSON inside a JSON string. It comes back raw with the
// model's key order kept, or as a JSON string when it is not JSON, and never
// as an error.
func TestToolArguments(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`"{\"b\":1,\"a\":\"x\"}"`, `{"b":1,"a":"x"}`},
		{`"ls -la"`, `"ls -la"`},
		{`"{\"cmd\":\"make && ./run > out"`, `"{\"cmd\":\"make && ./run > out"`}, // malformed; & and > as written
		{`""`, ""},
		{`null`, ""},
		{`{"b":1, "a":"x"}`, `{"b":1,"a":"x"}`},
	} {
		var call toolCall
		if err := json.Unmarshal([]byte(`{"function":{"arguments":`+c.in+`}}`), &call); err != nil {
			t.Errorf("arguments %s: %v", c.in, err)
			continue
		}
		got := call.Function.Arguments
		if string(got) != c.want || (c.want == "" && got != nil) {
			t.Errorf("arguments %s = %q, want %q", c.in, got, c.want)
		}
	}
}

// errorText draws an error item. Code and message read as one phrase, either
// one stands alone, and an item with neither is still drawn as something.
func TestErrorText(t *testing.T) {
	for _, c := range []struct {
		name          string
		code, message string
		want          string
	}{
		{"both", "model_error", "model failed", "model_error: model failed"},
		{"message only", "", "model failed", "model failed"},
		{"code only", "model_error", "", "model_error"},
		{"neither", "", "", "?"},
	} {
		if got := errorText(c.code, c.message); got != c.want {
			t.Errorf("%s: errorText(%q, %q) = %q, want %q", c.name, c.code, c.message, got, c.want)
		}
	}
}

func TestNumberText(t *testing.T) {
	for _, c := range []struct{ raw, want string }{
		{"", "0"},
		{"null", "0"},
		{"12", "12"},
		{"1.5", "1.5"},
		{`"nope"`, "0"},
	} {
		var raw json.RawMessage
		if c.raw != "" {
			raw = json.RawMessage(c.raw)
		}
		if got := numberText(raw, "0"); got != c.want {
			t.Errorf("numberText(%s) = %q, want %q", c.raw, got, c.want)
		}
	}
}
