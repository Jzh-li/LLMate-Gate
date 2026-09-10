package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newMockGateway 起一个最小网关替身：仅验证本包客户端与隐私端点的传输契约
// （Bearer 鉴权、json/text 双形态、占位符字面往返），不实现真实检测。
func newMockGateway(t *testing.T, token string) (*httptest.Server, *Client) {
	t.Helper()
	mux := http.NewServeMux()

	repl := func(s string) string {
		s = strings.ReplaceAll(s, "13800138000", "<<zh_phone_1>>")
		s = strings.ReplaceAll(s, "a@b.com", "<<email_1>>")
		return s
	}
	un := func(s string) string {
		s = strings.ReplaceAll(s, "<<zh_phone_1>>", "13800138000")
		s = strings.ReplaceAll(s, "<<email_1>>", "a@b.com")
		return s
	}

	mux.HandleFunc("/v1/privacy/redact", func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var in struct {
			JSON json.RawMessage `json:"json"`
			Text string          `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)

		var outJSON json.RawMessage
		var outText string
		changed := false
		if len(in.JSON) > 0 {
			out := repl(string(in.JSON))
			outJSON = json.RawMessage(out)
			changed = out != string(in.JSON)
		} else {
			outText = repl(in.Text)
			changed = outText != in.Text
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false) // 占位符必须字面输出
		_ = enc.Encode(map[string]interface{}{
			"json": outJSON, "text": outText, "request_id": "req-mock-1",
			"changed": changed, "strategy": "placeholder",
		})
	})

	mux.HandleFunc("/v1/privacy/restore", func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var in struct {
			JSON      json.RawMessage `json:"json"`
			Text      string          `json:"text"`
			RequestID string          `json:"request_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.RequestID == "" {
			http.Error(w, "request_id required", http.StatusBadRequest)
			return
		}
		var outJSON json.RawMessage
		var outText string
		if len(in.JSON) > 0 {
			outJSON = json.RawMessage(un(string(in.JSON)))
		} else {
			outText = un(in.Text)
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(map[string]interface{}{"json": outJSON, "text": outText})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, NewClient(srv.URL, token)
}

func TestClient_Anonymize_LiteralPlaceholder(t *testing.T) {
	srv, c := newMockGateway(t, "tok")
	_ = srv
	ctx := context.Background()

	in := json.RawMessage(`{"tool_input":{"command":"call 13800138000 and email a@b.com"}}`)
	resp, err := c.Anonymize(ctx, AnonymizeReq{JSON: in})
	if err != nil {
		t.Fatalf("anonymize: %v", err)
	}
	if !resp.Changed {
		t.Fatal("expected changed=true")
	}
	if !strings.Contains(string(resp.JSON), "<<zh_phone_1>>") {
		t.Fatalf("placeholder not literal in json: %s", resp.JSON)
	}
	if strings.Contains(string(resp.JSON), `\u003c`) {
		t.Fatalf("placeholder must not be escaped to \\u003c: %s", resp.JSON)
	}
	if resp.RequestID == "" {
		t.Fatal("request_id empty")
	}
}

func TestClient_Deanonymize_Roundtrip(t *testing.T) {
	_, c := newMockGateway(t, "tok")
	ctx := context.Background()

	in := json.RawMessage(`{"tool_input":{"command":"call 13800138000 and email a@b.com"}}`)
	red, err := c.Anonymize(ctx, AnonymizeReq{JSON: in})
	if err != nil {
		t.Fatalf("anonymize: %v", err)
	}
	rest, err := c.Deanonymize(ctx, DeanonymizeReq{JSON: red.JSON, RequestID: red.RequestID})
	if err != nil {
		t.Fatalf("deanonymize: %v", err)
	}
	if string(rest.JSON) != string(in) {
		t.Fatalf("roundtrip mismatch:\n got %s\nwant %s", rest.JSON, in)
	}
}

func TestClient_Deanonymize_MissingRequestID(t *testing.T) {
	srv, c := newMockGateway(t, "tok")
	_ = srv
	_, err := c.Deanonymize(context.Background(), DeanonymizeReq{JSON: json.RawMessage(`{"a":1}`)})
	if err == nil {
		t.Fatal("expected error for missing request_id")
	}
}

func TestClient_ScanToolParams(t *testing.T) {
	_, c := newMockGateway(t, "tok")
	ctx := context.Background()
	in := json.RawMessage(`{"x":"call 13800138000"}`)
	scan, err := c.ScanToolParams(ctx, ScanReq{JSON: in})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !scan.HasPII {
		t.Fatal("expected HasPII=true")
	}
	if scan.RequestID == "" {
		t.Fatal("request_id empty")
	}
	if strings.Contains(string(scan.RedactedJSON), `\u003c`) {
		t.Fatalf("scan redacted must be literal: %s", scan.RedactedJSON)
	}
	if scan.Recommendation != "redact" {
		t.Fatalf("recommendation=%q want redact", scan.Recommendation)
	}
}

func TestClient_TextMode(t *testing.T) {
	_, c := newMockGateway(t, "tok")
	ctx := context.Background()
	red, err := c.Anonymize(ctx, AnonymizeReq{Text: "mail a@b.com now"})
	if err != nil {
		t.Fatalf("anonymize text: %v", err)
	}
	if red.Text != "mail <<email_1>> now" {
		t.Fatalf("text redact mismatch: %q", red.Text)
	}
	rest, err := c.Deanonymize(ctx, DeanonymizeReq{Text: red.Text, RequestID: red.RequestID})
	if err != nil {
		t.Fatalf("deanonymize text: %v", err)
	}
	if rest.Text != "mail a@b.com now" {
		t.Fatalf("text restore mismatch: %q", rest.Text)
	}
}

func TestClient_AuthRequired(t *testing.T) {
	_, c := newMockGateway(t, "tok") // 网关要求 token=tok
	// 用错误令牌的客户端应收到 401 错误。
	bad := NewClient(c.GatewayURL(), "wrong")
	_, err := bad.Anonymize(context.Background(), AnonymizeReq{Text: "call 13800138000"})
	if err == nil {
		t.Fatal("expected auth error")
	}
}
