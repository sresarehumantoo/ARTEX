package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func newTestServer() (*Server, *httptest.Server) {
	s := New(ServerInfo{Name: "test", Version: "0.1"})
	ts := httptest.NewServer(s.Handler())
	return s, ts
}

func postRPC(url, sessionID string, req rpcRequest) (*http.Response, error) {
	body, _ := json.Marshal(req)
	r, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	if sessionID != "" {
		r.Header.Set("Mcp-Session-Id", sessionID)
	}
	return http.DefaultClient.Do(r)
}

func decodeRPC(resp *http.Response) (rpcResponse, error) {
	defer resp.Body.Close()
	var out rpcResponse
	err := json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}

func initialize(t *testing.T, url string) string {
	t.Helper()
	id := 1
	resp, err := postRPC(url, "", rpcRequest{JSONRPC: "2.0", ID: &id, Method: "initialize"})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize did not return a session id")
	}
	rpc, err := decodeRPC(resp)
	if err != nil {
		t.Fatalf("decode initialize response: %v", err)
	}
	if rpc.Error != nil {
		t.Fatalf("initialize returned error: %v", rpc.Error)
	}
	return sid
}

func TestInitializeCreatesSession(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()

	sid := initialize(t, ts.URL)
	if s.sessions.Count() != 1 {
		t.Fatalf("expected 1 session, got %d", s.sessions.Count())
	}
	sess := s.sessions.Get(sid)
	if sess == nil {
		t.Fatal("session not found by returned id")
	}
}

func TestRequestWithoutSessionFails(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	id := 1
	resp, err := postRPC(ts.URL, "", rpcRequest{JSONRPC: "2.0", ID: &id, Method: "tools/list"})
	if err != nil {
		t.Fatal(err)
	}
	rpc, _ := decodeRPC(resp)
	if rpc.Error == nil {
		t.Fatal("expected error for request without session")
	}
	if rpc.Error.Code != -32600 {
		t.Fatalf("expected code -32600, got %d", rpc.Error.Code)
	}
}

func TestRequestWithBadSessionFails(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	id := 1
	resp, err := postRPC(ts.URL, "nonexistent", rpcRequest{JSONRPC: "2.0", ID: &id, Method: "tools/list"})
	if err != nil {
		t.Fatal(err)
	}
	rpc, _ := decodeRPC(resp)
	if rpc.Error == nil {
		t.Fatal("expected error for bad session")
	}
}

func TestToolsList(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	sid := initialize(t, ts.URL)
	id := 2
	resp, err := postRPC(ts.URL, sid, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "tools/list"})
	if err != nil {
		t.Fatal(err)
	}
	rpc, _ := decodeRPC(resp)
	if rpc.Error != nil {
		t.Fatalf("tools/list error: %v", rpc.Error)
	}
	raw, _ := json.Marshal(rpc.Result)
	var result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	json.Unmarshal(raw, &result)
	found := false
	for _, tool := range result.Tools {
		if tool.Name == "list_sessions" {
			found = true
		}
	}
	if !found {
		t.Fatal("list_sessions tool not found in tools/list")
	}
}

func TestListSessionsTool(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	sid1 := initialize(t, ts.URL)
	sid2 := initialize(t, ts.URL)

	id := 3
	params, _ := json.Marshal(map[string]any{"name": "list_sessions", "arguments": map[string]any{}})
	resp, err := postRPC(ts.URL, sid1, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "tools/call", Params: params})
	if err != nil {
		t.Fatal(err)
	}
	rpc, _ := decodeRPC(resp)
	if rpc.Error != nil {
		t.Fatalf("tools/call error: %v", rpc.Error)
	}
	raw, _ := json.Marshal(rpc.Result)
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	json.Unmarshal(raw, &result)
	if len(result.Content) == 0 {
		t.Fatal("empty content in list_sessions result")
	}
	text := result.Content[0].Text
	if !strings.Contains(text, sid1) || !strings.Contains(text, sid2) {
		t.Fatalf("list_sessions result does not contain both session ids: %s", text)
	}
	if !strings.Contains(text, `"count":2`) {
		t.Fatalf("expected count 2 in result: %s", text)
	}
}

func TestDeleteSession(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()

	sid := initialize(t, ts.URL)
	if s.sessions.Count() != 1 {
		t.Fatal("expected 1 session")
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL, nil)
	req.Header.Set("Mcp-Session-Id", sid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
	if s.sessions.Count() != 0 {
		t.Fatal("session not removed")
	}
}

func TestDeleteNonexistentSession(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL, nil)
	req.Header.Set("Mcp-Session-Id", "bogus")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestCustomToolCall(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()

	s.RegisterTool(Tool{
		Name:        "echo",
		Description: "echo input back",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"msg": map[string]any{"type": "string"}}},
		Handler: func(_ context.Context, sess *Session, input json.RawMessage) (ToolResult, error) {
			var a struct{ Msg string }
			json.Unmarshal(input, &a)
			sess.Set("last_echo", a.Msg)
			return TextResult("echo: " + a.Msg), nil
		},
	})

	sid := initialize(t, ts.URL)
	id := 10
	params, _ := json.Marshal(map[string]any{"name": "echo", "arguments": map[string]any{"msg": "hello"}})
	resp, err := postRPC(ts.URL, sid, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "tools/call", Params: params})
	if err != nil {
		t.Fatal(err)
	}
	rpc, _ := decodeRPC(resp)
	if rpc.Error != nil {
		t.Fatalf("tools/call error: %v", rpc.Error)
	}
	raw, _ := json.Marshal(rpc.Result)
	if !strings.Contains(string(raw), "echo: hello") {
		t.Fatalf("unexpected result: %s", raw)
	}

	sess := s.sessions.Get(sid)
	if sess.Get("last_echo") != "hello" {
		t.Fatal("session context not updated by tool handler")
	}
}

func TestUnknownToolCall(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	sid := initialize(t, ts.URL)
	id := 11
	params, _ := json.Marshal(map[string]any{"name": "nonexistent", "arguments": map[string]any{}})
	resp, err := postRPC(ts.URL, sid, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "tools/call", Params: params})
	if err != nil {
		t.Fatal(err)
	}
	rpc, _ := decodeRPC(resp)
	if rpc.Error == nil || rpc.Error.Code != -32602 {
		t.Fatalf("expected -32602 for unknown tool, got %+v", rpc.Error)
	}
}

func TestUnknownMethodFails(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	sid := initialize(t, ts.URL)
	id := 12
	resp, err := postRPC(ts.URL, sid, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "resources/list"})
	if err != nil {
		t.Fatal(err)
	}
	rpc, _ := decodeRPC(resp)
	if rpc.Error == nil || rpc.Error.Code != -32601 {
		t.Fatalf("expected -32601 for unknown method, got %+v", rpc.Error)
	}
}

func TestConcurrentClients(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()

	s.RegisterTool(Tool{
		Name:        "set_name",
		Description: "store a name in the session context",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}},
		Handler: func(_ context.Context, sess *Session, input json.RawMessage) (ToolResult, error) {
			var a struct{ Name string }
			json.Unmarshal(input, &a)
			sess.Set("name", a.Name)
			return TextResult("ok"), nil
		},
	})
	s.RegisterTool(Tool{
		Name:        "get_name",
		Description: "retrieve the name from the session context",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Handler: func(_ context.Context, sess *Session, _ json.RawMessage) (ToolResult, error) {
			return TextResult(sess.Get("name")), nil
		},
	})

	const clients = 10
	var wg sync.WaitGroup
	errors := make(chan string, clients)

	for i := range clients {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			name := fmt.Sprintf("client-%d", n)

			sid := func() string {
				id := 1
				resp, err := postRPC(ts.URL, "", rpcRequest{JSONRPC: "2.0", ID: &id, Method: "initialize"})
				if err != nil {
					errors <- fmt.Sprintf("client %d initialize: %v", n, err)
					return ""
				}
				s := resp.Header.Get("Mcp-Session-Id")
				resp.Body.Close()
				return s
			}()
			if sid == "" {
				return
			}

			id := 2
			params, _ := json.Marshal(map[string]any{"name": "set_name", "arguments": map[string]any{"name": name}})
			resp, err := postRPC(ts.URL, sid, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "tools/call", Params: params})
			if err != nil {
				errors <- fmt.Sprintf("client %d set_name: %v", n, err)
				return
			}
			resp.Body.Close()

			id = 3
			params, _ = json.Marshal(map[string]any{"name": "get_name", "arguments": map[string]any{}})
			resp, err = postRPC(ts.URL, sid, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "tools/call", Params: params})
			if err != nil {
				errors <- fmt.Sprintf("client %d get_name: %v", n, err)
				return
			}
			rpc, _ := decodeRPC(resp)
			raw, _ := json.Marshal(rpc.Result)
			if !strings.Contains(string(raw), name) {
				errors <- fmt.Sprintf("client %d: got %s, want %s", n, raw, name)
			}
		}(i)
	}
	wg.Wait()
	close(errors)
	for e := range errors {
		t.Error(e)
	}

	if s.sessions.Count() != clients {
		t.Fatalf("expected %d sessions, got %d", clients, s.sessions.Count())
	}
}

func TestOversizedRequest(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	body := strings.Repeat("x", maxRequestBytes+1)
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, b)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", resp.StatusCode)
	}
}

func TestNotificationAccepted(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	sid := initialize(t, ts.URL)
	resp, err := postRPC(ts.URL, sid, rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", resp.StatusCode)
	}
}

func TestSessionIsolation(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()

	s.RegisterTool(Tool{
		Name:        "store",
		Description: "store a value",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"v": map[string]any{"type": "string"}}},
		Handler: func(_ context.Context, sess *Session, input json.RawMessage) (ToolResult, error) {
			var a struct{ V string }
			json.Unmarshal(input, &a)
			sess.Set("val", a.V)
			return TextResult("stored"), nil
		},
	})
	s.RegisterTool(Tool{
		Name:        "load",
		Description: "load the stored value",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Handler: func(_ context.Context, sess *Session, _ json.RawMessage) (ToolResult, error) {
			return TextResult(sess.Get("val")), nil
		},
	})

	sid1 := initialize(t, ts.URL)
	sid2 := initialize(t, ts.URL)

	callTool := func(sid, tool string, args map[string]any) string {
		t.Helper()
		id := 100
		params, _ := json.Marshal(map[string]any{"name": tool, "arguments": args})
		resp, err := postRPC(ts.URL, sid, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "tools/call", Params: params})
		if err != nil {
			t.Fatal(err)
		}
		rpc, _ := decodeRPC(resp)
		raw, _ := json.Marshal(rpc.Result)
		var result struct {
			Content []struct{ Text string } `json:"content"`
		}
		json.Unmarshal(raw, &result)
		if len(result.Content) == 0 {
			return ""
		}
		return result.Content[0].Text
	}

	callTool(sid1, "store", map[string]any{"v": "alpha"})
	callTool(sid2, "store", map[string]any{"v": "beta"})

	got1 := callTool(sid1, "load", nil)
	got2 := callTool(sid2, "load", nil)

	if got1 != "alpha" {
		t.Fatalf("session 1 isolation broken: got %q, want alpha", got1)
	}
	if got2 != "beta" {
		t.Fatalf("session 2 isolation broken: got %q, want beta", got2)
	}
}

func TestDeleteMissingHeader(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestMalformedJSON(t *testing.T) {
	_, ts := newTestServer()
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json", strings.NewReader("{bad"))
	if err != nil {
		t.Fatal(err)
	}
	rpc, _ := decodeRPC(resp)
	if rpc.Error == nil || rpc.Error.Code != -32700 {
		t.Fatalf("expected parse error, got %+v", rpc.Error)
	}
}

func TestToolHandlerError(t *testing.T) {
	s, ts := newTestServer()
	defer ts.Close()

	s.RegisterTool(Tool{
		Name:        "fail",
		Description: "always fails",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(_ context.Context, _ *Session, _ json.RawMessage) (ToolResult, error) {
			return ToolResult{}, fmt.Errorf("something broke")
		},
	})

	sid := initialize(t, ts.URL)
	id := 20
	params, _ := json.Marshal(map[string]any{"name": "fail", "arguments": map[string]any{}})
	resp, err := postRPC(ts.URL, sid, rpcRequest{JSONRPC: "2.0", ID: &id, Method: "tools/call", Params: params})
	if err != nil {
		t.Fatal(err)
	}
	rpc, _ := decodeRPC(resp)
	if rpc.Error != nil {
		t.Fatalf("expected result with isError, got rpc error: %v", rpc.Error)
	}
	raw, _ := json.Marshal(rpc.Result)
	var result struct {
		Content []struct{ Text string } `json:"content"`
		IsError bool                    `json:"isError"`
	}
	json.Unmarshal(raw, &result)
	if !result.IsError {
		t.Fatal("expected isError=true")
	}
	if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "something broke") {
		t.Fatalf("unexpected error content: %s", raw)
	}
}
