package llm

import (
	"context"
	"strings"

	"github.com/wannacry39/agent_nirs/internal/egress"
)

func (c *Client) Destination() egress.Destination {
	return egress.Destination{Kind: "llm", Address: strings.TrimRight(c.BaseURL, "/") + "/chat/completions"}
}

// modelAttempt читает окончательное тело, но маркирует только копию разбора.
// JSON Schema и вложенный tool call не меняют назначение внешнего запроса.
func (c *Client) modelAttempt(body []byte, d egress.Destination) (egress.Attempt, error) {
	v, err := c.Guard.Decode(body)
	if err != nil {
		return egress.Attempt{}, err
	}
	root, ok := v.(map[string]any)
	if !ok {
		return egress.Attempt{}, egress.ErrConfig
	}
	if list, ok := root["tools"].([]any); ok {
		for _, item := range list {
			tool, ok := item.(map[string]any)
			if !ok || tool["type"] != "function" {
				continue
			}
			f, ok := tool["function"].(map[string]any)
			if !ok {
				continue
			}
			if schema, ok := f["parameters"]; ok {
				f["parameters"] = egress.Fragment{Kind: egress.Schema, Value: schema}
			}
		}
	}
	if messages, ok := root["messages"].([]any); ok {
		for _, item := range messages {
			msg, ok := item.(map[string]any)
			if !ok {
				continue
			}
			calls, ok := msg["tool_calls"].([]any)
			if !ok {
				continue
			}
			for _, item := range calls {
				call, ok := item.(map[string]any)
				if !ok {
					continue
				}
				f, ok := call["function"].(map[string]any)
				if !ok {
					continue
				}
				if args, ok := f["arguments"].(string); ok && strings.TrimSpace(args) != "" {
					value, e := c.Guard.Decode([]byte(args))
					if e != nil {
						return egress.Attempt{}, e
					}
					f["arguments"] = egress.Fragment{Kind: egress.Data, Value: value}
				}
			}
		}
	}
	return egress.Attempt{Destination: d, Operation: egress.ModelOperation(), Fragments: []egress.Fragment{{Value: root}}}, nil
}
func (c *Client) checkBody(ctx context.Context, body []byte, d egress.Destination) error {
	a, err := c.modelAttempt(body, d)
	if err != nil {
		return err
	}
	if c.Project != "" {
		a.Fragments = append(a.Fragments, egress.Fragment{Value: c.Project, Path: egress.Path{egress.Index(-1), egress.Key("OpenAI-Project")}})
	}
	return c.Guard.Check(ctx, a, c.APIKey)

}
