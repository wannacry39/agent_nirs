package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/wannacry39/agent_nirs/internal/tools"
)

// ServerTool — инструмент MCP-сервера: описание и реализация.
type ServerTool struct {
	Info    ToolInfo
	Handler func(ctx context.Context, args map[string]any) (string, error)
}

// Server — MCP-сервер: сервер, который предоставляет функции стороннего
// сервиса в виде инструментов по MCP и сам выполняет их вызовы. При
// подключении сервер передаёт список этих инструментов, а получив вызов,
// выполняет нужный инструмент и присылает ответ.
//
// Сервер принимает POST-запросы с JSON-RPC 2.0 и отвечает JSON.
// Это подмножество транспорта Streamable HTTP из спецификации MCP:
// потоковые ответы (SSE) сервер не использует.
type Server struct {
	name, version string

	mu       sync.Mutex
	tools    []ServerTool
	sessions map[string]time.Time // сессия → время последнего обращения
}

// SessionTTL — через сколько без обращений сессия забывается.
const SessionTTL = 30 * time.Minute

// NewServer создаёт MCP-сервер.
func NewServer(name, version string) *Server {
	return &Server{name: name, version: version, sessions: map[string]time.Time{}}
}

// AddTool регистрирует инструмент.
func (s *Server) AddTool(tool ServerTool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = append(s.tools, tool)
}

// ServeHTTP обрабатывает одно сообщение JSON-RPC.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Спецификация требует проверять Origin: иначе страница в браузере
	// может обратиться к локальному серверу (DNS rebinding).
	if !allowedOrigin(r.Header.Get("Origin")) {
		http.Error(w, "запрещённый Origin", http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete: // клиент закрывает сессию
		id := r.Header.Get(headerSessionID)
		if id == "" {
			http.Error(w, "нет заголовка "+headerSessionID, http.StatusBadRequest)
			return
		}
		if !s.touchSession(id) {
			http.Error(w, "неизвестная сессия", http.StatusNotFound)
			return
		}
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
		log.Print("[MCP-сервер] сессия закрыта")
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		// Отдельный поток событий (GET + SSE) этот сервер не поддерживает,
		// спецификация разрешает в этом случае отвечать 405.
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, Response{JSONRPC: "2.0", Error: &RPCError{codeParseError, "некорректный JSON"}})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" || string(req.ID) == "null" {
		writeJSON(w, Response{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{codeInvalidRequest, "некорректный запрос JSON-RPC 2.0"}})
		return
	}

	// Всё, кроме initialize, выполняется только в известной сессии.
	if req.Method != "initialize" {
		id := r.Header.Get(headerSessionID)
		if id == "" {
			http.Error(w, "нет заголовка "+headerSessionID+", сначала вызовите initialize", http.StatusBadRequest)
			return
		}
		if !s.touchSession(id) {
			http.Error(w, "неизвестная сессия, вызовите initialize заново", http.StatusNotFound)
			return
		}
	}

	// Уведомление (без ID): ответ не нужен.
	if req.IsNotification() {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, rpcErr := s.handle(r.Context(), w, req)
	resp := Response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr}
	if rpcErr == nil {
		resp.Result, _ = json.Marshal(result)
	}
	writeJSON(w, resp)
}

func (s *Server) handle(ctx context.Context, w http.ResponseWriter, req Request) (any, *RPCError) {
	switch req.Method {
	case "initialize":
		var params initializeParams
		if err := json.Unmarshal(req.Params, &params); err != nil || params.ProtocolVersion == "" || params.Capabilities == nil || params.ClientInfo.Name == "" || params.ClientInfo.Version == "" {
			return nil, &RPCError{codeInvalidParams, "initialize: нужны protocolVersion, capabilities и clientInfo"}
		}
		sessionID := s.newSession()
		w.Header().Set(headerSessionID, sessionID)
		log.Print("[MCP-сервер] новая сессия")
		return initializeResult{
			ProtocolVersion: ProtocolVersion,
			Capabilities:    map[string]any{"tools": map[string]any{}},
			ServerInfo:      implementation{s.name, s.version},
		}, nil

	case "ping":
		return map[string]any{}, nil

	case "tools/list":
		// Инструментов мало, поэтому список отдаётся одной страницей
		// (без nextCursor).
		s.mu.Lock()
		infos := make([]ToolInfo, 0, len(s.tools))
		for _, tool := range s.tools {
			infos = append(infos, tool.Info)
		}
		s.mu.Unlock()
		log.Printf("[MCP-сервер] tools/list: отдаю %d инструментов", len(infos))
		return listToolsResult{Tools: infos}, nil

	case "tools/call":
		var params callToolParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &RPCError{codeInvalidParams, "некорректные параметры"}
		}
		tool, ok := s.findTool(params.Name)
		if !ok {
			return nil, &RPCError{codeInvalidParams, "неизвестный инструмент: " + params.Name}
		}
		if err := tools.ValidateArguments(tool.Info.InputSchema, params.Arguments); err != nil {
			return nil, &RPCError{codeInvalidParams, "некорректные параметры: " + err.Error()}
		}
		log.Print("[MCP-сервер] tools/call: выполняю инструмент (аргументы скрыты)")
		text, err := tool.Handler(ctx, params.Arguments)
		if err != nil {
			// Ошибка выполнения — это результат инструмента, а не ошибка протокола.
			return callToolResult{Content: []Content{{Type: "text", Text: err.Error()}}, IsError: true}, nil
		}
		return callToolResult{Content: []Content{{Type: "text", Text: text}}}, nil

	default:
		return nil, &RPCError{codeMethodNotFound, "метод не поддерживается: " + req.Method}
	}
}

func (s *Server) findTool(name string) (ServerTool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, tool := range s.tools {
		if tool.Info.Name == name {
			return tool, true
		}
	}
	return ServerTool{}, false
}

// newSession создаёт сессию и заодно забывает истёкшие.
func (s *Server) newSession() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	id := hex.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for old, seen := range s.sessions {
		if now.Sub(seen) > SessionTTL {
			delete(s.sessions, old)
		}
	}
	s.sessions[id] = now
	return id
}

// touchSession проверяет, что сессия известна и не истекла, и продлевает её.
func (s *Server) touchSession(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen, ok := s.sessions[id]
	if !ok {
		return false
	}
	if time.Since(seen) > SessionTTL {
		delete(s.sessions, id)
		return false
	}
	s.sessions[id] = time.Now()
	return true
}

// allowedOrigin разрешает запросы без Origin (обычные программы)
// и со страниц на этой же машине.
func allowedOrigin(origin string) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
