// Package mcpserver implements an MCP server over the Streamable HTTP
// transport with per-client session tracking. Multiple HTTP clients connect
// simultaneously, each receiving a unique session ID and its own engagement
// context. The shared data store (PostgreSQL) handles concurrency itself.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

const (
	protocolVersion = "2025-03-26"
	maxRequestBytes = 4 << 20
)

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ToolResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Handler     func(ctx context.Context, session *Session, input json.RawMessage) (ToolResult, error)
}

type Server struct {
	info     ServerInfo
	sessions *SessionManager

	mu    sync.RWMutex
	tools map[string]Tool
}

func New(info ServerInfo) *Server {
	s := &Server{
		info:     info,
		sessions: NewSessionManager(),
		tools:    make(map[string]Tool),
	}
	s.registerBuiltinTools()
	return s
}

func (s *Server) Sessions() *SessionManager { return s.sessions }

func (s *Server) RegisterTool(t Tool) {
	s.mu.Lock()
	s.tools[t.Name] = t
	s.mu.Unlock()
}

func (s *Server) registerBuiltinTools() {
	s.RegisterTool(Tool{
		Name:        "list_sessions",
		Description: "List all active MCP client sessions connected to this server.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Handler: func(_ context.Context, _ *Session, _ json.RawMessage) (ToolResult, error) {
			sessions := s.sessions.List()
			out, _ := json.Marshal(map[string]any{
				"count":    len(sessions),
				"sessions": sessions,
			})
			return ToolResult{Content: []ContentBlock{{Type: "text", Text: string(out)}}}, nil
		},
	})
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			s.handlePost(w, r)
		case http.MethodDelete:
			s.handleDelete(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	sid := r.Header.Get("Mcp-Session-Id")
	if sid == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	if !s.sessions.Remove(sid) {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}
	if len(body) > maxRequestBytes {
		writeRPCError(w, nil, -32700, "request too large")
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, nil, -32700, "parse error: "+err.Error())
		return
	}

	if req.Method == "initialize" {
		s.handleInitialize(w, &req)
		return
	}

	sid := r.Header.Get("Mcp-Session-Id")
	session := s.sessions.Get(sid)
	if session == nil {
		writeRPCError(w, req.ID, -32600, "invalid or missing session id")
		return
	}
	session.touch()

	switch req.Method {
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		s.handleToolsList(w, &req)
	case "tools/call":
		s.handleToolsCall(w, r, &req, session)
	default:
		writeRPCError(w, req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func (s *Server) handleInitialize(w http.ResponseWriter, req *rpcRequest) {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if req.Params != nil {
		_ = json.Unmarshal(req.Params, &params)
	}

	session := s.sessions.Create()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Mcp-Session-Id", session.ID)
	json.NewEncoder(w).Encode(rpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": s.info,
		},
	})
}

func (s *Server) handleToolsList(w http.ResponseWriter, req *rpcRequest) {
	s.mu.RLock()
	tools := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		tools = append(tools, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  map[string]any{"tools": tools},
	})
}

func (s *Server) handleToolsCall(w http.ResponseWriter, r *http.Request, req *rpcRequest, session *Session) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if req.Params != nil {
		_ = json.Unmarshal(req.Params, &params)
	}

	s.mu.RLock()
	tool, ok := s.tools[params.Name]
	s.mu.RUnlock()
	if !ok {
		writeRPCError(w, req.ID, -32602, fmt.Sprintf("unknown tool: %s", params.Name))
		return
	}

	result, err := tool.Handler(r.Context(), session, params.Arguments)
	if err != nil {
		result = ToolResult{
			Content: []ContentBlock{{Type: "text", Text: "Error: " + err.Error()}},
			IsError: true,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	})
}

func writeRPCError(w http.ResponseWriter, id *int, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	status := http.StatusOK
	if code == -32700 {
		status = http.StatusBadRequest
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg},
	})
}

func textResult(text string) ToolResult {
	return ToolResult{Content: []ContentBlock{{Type: "text", Text: text}}}
}

func jsonResult(v any) (ToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return ToolResult{}, err
	}
	return textResult(string(b)), nil
}

// TextResult and JSONResult are public helpers for external tool handlers.
func TextResult(text string) ToolResult { return textResult(text) }
func JSONResult(v any) (ToolResult, error) { return jsonResult(v) }

// parseAccept reports whether the client accepts json or SSE. The server only
// responds with JSON for now; this exists so we can extend later.
func parseAccept(accept string) (json bool, sse bool) {
	for _, part := range strings.Split(accept, ",") {
		mt := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		switch mt {
		case "application/json", "*/*":
			json = true
		case "text/event-stream":
			sse = true
		}
	}
	return
}
