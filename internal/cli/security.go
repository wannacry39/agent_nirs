package cli

import (
	"fmt"
	"os"

	"github.com/wannacry39/agent_nirs/internal/egress"
	"github.com/wannacry39/agent_nirs/internal/llm"
)

// LoadSecurity вызывается до сетевых подключений. Указанная, но ошибочная
// политика никогда не превращается в незащищённый режим.
func LoadSecurity() (*egress.Guard, error) {
	path := os.Getenv("AGENT_EGRESS_POLICY")
	if path == "" {
		fmt.Fprintln(os.Stderr, "[egress] контроль исходящих данных выключен (учебный режим)")
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, egress.ErrConfig
	}
	defer f.Close()
	g, err := egress.Load(f, os.LookupEnv, os.Stderr, os.Getenv("LLM_API_KEY"))
	if err != nil {
		return nil, egress.ErrConfig
	}
	fmt.Fprintln(os.Stderr, "[egress] контроль исходящих данных включён")
	return g, nil
}
func BindModel(model *llm.Client, g *egress.Guard) error {
	model.Guard = g
	if g != nil {
		return g.Validate(model.Destination(), egress.ModelOperation())
	}
	return nil
}
func ValidateSkills(g *egress.Guard, enabled bool) error {
	if g != nil && enabled {
		return g.Validate(egress.SkillDestination(), egress.ToolOperation("load_skill"))
	}
	return nil
}
