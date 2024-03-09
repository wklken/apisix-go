package base

type BasePlugin struct {
	Name     string
	Priority int
	Schema   string
}

func (p *BasePlugin) GetName() string {
	return p.Name
}

func (p *BasePlugin) GetPriority() int {
	return p.Priority
}

func (p *BasePlugin) GetSchema() string {
	return p.Schema
}
