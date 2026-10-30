package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/wannacry39/agent_nirs/internal/egress"
	"github.com/wannacry39/agent_nirs/internal/tools"
)

// Client — MCP-клиент: компонент приложения, работающего с LLM, через
// который оно связано с MCP-сервером. Клиент подключается к серверу,
// получает от него список инструментов, отправляет ему вызовы по MCP
// и принимает ответы.
//
// Client реализует tools.Toolbox, поэтому харнесс работает с MCP-инструментами
// так же, как с локальными. Разница только в том, где выполняется вызов:
// локальный инструмент выполняет харнесс, MCP-инструмент — MCP-сервер.
type Client struct {
	url         string
	transport   Transport
	guard       *egress.Guard
	operationMu sync.Mutex // вызов не должен попасть в середину переподключения

	mu     sync.Mutex
	nextID int
	tools  []tools.Tool
	// Каталог фиксируется при первом подключении: харнесс проверяет права
	// по этим описаниям. Изменившийся каталог требует нового клиента и харнесса.
	catalogLoaded  bool
	catalogChanged bool
}

// ErrToolsChanged запрещает вызовы с описаниями, по которым харнесс уже не
// может проверить актуальные права и параметры.
var ErrToolsChanged = errors.New("каталог MCP-инструментов изменился; перезапустите агент, чтобы заново проверить инструменты и права")

// NewClient создаёт клиент для MCP-сервера по адресу url (транспорт HTTP).
func NewClient(url string) *Client {
	return &Client{url: url, transport: NewHTTPTransport(url)}
}

// Connect — подключение к серверу: initialize, затем список инструментов.
func (c *Client) Connect(ctx context.Context) (err error) {
	defer func() {
		if c.guard != nil {
			err = egress.SafeError(err)
		}
	}()
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if err := c.initialize(ctx); err != nil {
		return err
	}
	return c.listTools(ctx)
}

// initialize — знакомство с сервером, после него сервер выдаёт сессию.
func (c *Client) initialize(ctx context.Context) error {
	raw, err := c.send(ctx, "initialize", initializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo:      implementation{"agent_nirs", "1.0"},
	})
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	var result initializeResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("initialize: разбор ответа: %w", err)
	}
	if result.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("initialize: версия MCP %q не поддерживается (ожидалась %s)", result.ProtocolVersion, ProtocolVersion)
	}
	if _, ok := result.Capabilities["tools"].(map[string]any); !ok {
		return errors.New("initialize: сервер не объявил возможность tools")
	}
	notification := Request{JSONRPC: "2.0", Method: "notifications/initialized"}
	if c.guard != nil {
		if err := c.checkParams(ctx, notification.Method, []byte(`{}`)); err != nil {
			return err
		}
	}
	if _, err := c.transport.Send(ctx, notification); err != nil {
		return fmt.Errorf("notifications/initialized: %w", err)
	}
	return nil
}

// listTools получает список инструментов (с учётом страниц) и переводит
// описания из формата MCP в общий формат инструментов.
func (c *Client) listTools(ctx context.Context) error {
	var converted []tools.Tool
	cursor := ""
	seen := map[string]bool{}
	for {
		raw, err := c.request(ctx, "tools/list", listToolsParams{Cursor: cursor})
		if err != nil {
			return fmt.Errorf("tools/list: %w", err)
		}
		var page listToolsResult
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("разбор tools/list: %w", err)
		}
		for _, info := range page.Tools {
			converted = append(converted, tools.Tool{
				Name:        info.Name,
				Description: info.Description,
				InputSchema: info.InputSchema,
				Dangerous:   info.Dangerous(),
				Destination: c.destination(),
			})
		}
		if page.NextCursor == "" {
			break
		}
		if seen[page.NextCursor] {
			return fmt.Errorf("tools/list: сервер повторил курсор %q", page.NextCursor)
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
	// Порядок в tools/list не меняет смысл каталога.
	sort.Slice(converted, func(i, j int) bool { return converted[i].Name < converted[j].Name })
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.catalogChanged || (c.catalogLoaded && !reflect.DeepEqual(c.tools, converted)) {
		c.catalogChanged = true
		return ErrToolsChanged
	}
	c.tools = converted
	c.catalogLoaded = true
	return nil
}

// Name — для логов харнесса.
func (c *Client) Name() string { return "MCP-сервер " + c.url }

// Tools возвращает копию списка инструментов, полученного от сервера.
func (c *Client) Tools() []tools.Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]tools.Tool(nil), c.tools...)
}

// Call отправляет серверу tools/call. Выполняет инструмент сервер.
func (c *Client) Call(ctx context.Context, name string, args map[string]any) tools.Result {
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	c.mu.Lock()
	changed := c.catalogChanged
	c.mu.Unlock()
	if changed {
		return tools.Result{Content: ErrToolsChanged.Error(), IsError: true}
	}
	raw, err := c.request(ctx, "tools/call", callToolParams{Name: name, Arguments: args})
	if err != nil {
		if c.guard != nil {
			err = egress.SafeError(err)
		}
		return tools.Result{Content: "ошибка MCP: " + err.Error(), IsError: true}
	}
	var result callToolResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return tools.Result{Content: "ошибка разбора ответа MCP: " + err.Error(), IsError: true}
	}
	var texts []string
	for _, part := range result.Content {
		if part.Type == "text" {
			texts = append(texts, part.Text)
		}
	}
	return tools.Result{Content: strings.Join(texts, "\n"), IsError: result.IsError}
}

// Close закрывает сессию на сервере.
func (c *Client) Close(ctx context.Context) {
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if c.guard != nil {
		if c.checkParams(ctx, "close", []byte(`{}`)) != nil {
			return
		}
	}
	c.transport.Close(ctx)
}

// request отправляет запрос. Если сервер забыл сессию, клиент по спецификации
// подключается заново (initialize), заново получает список инструментов
// Повтор допустим только при неизменном каталоге: иначе права и схемы,
// которые проверил харнесс, уже устарели. Последующие вызовы тоже блокируются.
func (c *Client) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	result, err := c.send(ctx, method, params)
	if !errors.Is(err, ErrSessionExpired) {
		return result, err
	}
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	if method != "tools/list" {
		if err := c.listTools(ctx); err != nil {
			return nil, err
		}
	}
	return c.send(ctx, method, params)
}

// send отправляет запрос JSON-RPC с новым ID и возвращает результат.
func (c *Client) send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	rawParams, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	if c.guard != nil {
		if err := c.checkParams(ctx, method, rawParams); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	c.nextID++
	id := json.RawMessage(strconv.Itoa(c.nextID))
	c.mu.Unlock()

	resp, err := c.transport.Send(ctx, Request{JSONRPC: "2.0", ID: id, Method: method, Params: rawParams})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("ошибка JSON-RPC %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}
