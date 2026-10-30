package mcp

import (
	"context"
	"net/http"

	"github.com/wannacry39/agent_nirs/internal/egress"
)

// NewProtectedClient подключает Guard до первого initialize, а не после Connect.
func NewProtectedClient(url string, g *egress.Guard) (*Client, error) {
	d := egress.Destination{Kind: "mcp", Address: url}
	for _, method := range []string{"initialize", "notifications/initialized", "tools/list", "close"} {
		if err := g.Validate(d, egress.ControlOperation(method)); err != nil {
			return nil, egress.ErrConfig
		}
	}
	c := NewClient(url)
	c.guard = g
	t := c.transport.(*HTTPTransport)
	t.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c, nil
}
func (c *Client) EgressGuard() *egress.Guard { return c.guard }
func (c *Client) destination() egress.Destination {
	return egress.Destination{Kind: "mcp", Address: c.url}
}

func (c *Client) checkParams(ctx context.Context, method string, raw []byte) error {
	value, err := c.guard.Decode(raw)
	if err != nil {
		return err
	}
	a := egress.Attempt{Destination: c.destination(), Operation: egress.ControlOperation(method), Fragments: []egress.Fragment{{Value: value}}}
	if method == "tools/call" {
		params, ok := value.(map[string]any)
		if !ok {
			return egress.ErrConfig
		}
		name, ok := params["name"].(string)
		if !ok {
			return egress.ErrConfig
		}
		a = egress.ToolAttempt(c.destination(), name, params["arguments"])
		for k, v := range params {
			if k != "name" && k != "arguments" {
				// Отрицательный индекс не может быть путём аргумента или разрешением политики.
				a.Fragments = append(a.Fragments, egress.Fragment{Value: v, Path: egress.Path{egress.Index(-1), egress.Key(k)}})
			}
		}
	}
	return c.guard.Check(ctx, a)
}
