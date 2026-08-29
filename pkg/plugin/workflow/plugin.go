package workflow

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/plugin/limit_conn"
	"github.com/wklken/apisix-go/pkg/plugin/limit_count"
	"github.com/wklken/apisix-go/pkg/plugin/limit_req"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config           Config
	enabledChecker   func(string) bool
	childStoppers    []workflowChildStopper
	childOwners      []base.PreparedCompositeChild
	children         map[actionPosition]workflowChild
	route            resource.Route
	service          resource.Service
	resourceSet      bool
	lifecycleMu      sync.Mutex
	preparationEpoch uint64
	preparingToken   uint64
	resourceEpoch    uint64
	current          *workflowGeneration
	publicConfig     atomic.Pointer[Config]
	sourceConfig     Config
	sourceConfigSet  bool
}

type workflowGeneration struct {
	config          Config
	children        map[actionPosition]workflowChild
	stoppers        []workflowChildStopper
	owners          []base.PreparedCompositeChild
	active          int
	contextUpdating bool
	contextReady    chan struct{}
	retired         bool
	closeScheduled  bool
}

type workflowRetirement struct {
	stoppers []workflowChildStopper
	owners   []base.PreparedCompositeChild
}

type actionPosition struct {
	rule   int
	action int
}

type workflowChild interface {
	Init() error
	PostInit() error
	Config() any
	GetSchema() string
}

type workflowChildStopper interface {
	Stop()
}

const (
	priority = 1006
	name     = "workflow"
)

var errWorkflowChildPreparation = errors.New("workflow child preparation failed")

var errWorkflowLifecycleBusy = errors.New("workflow lifecycle operation in progress")

const schema = `
{
  "type": "object",
  "properties": {
    "rules": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "case": {
            "type": "array",
            "items": {
              "anyOf": [
                {
                  "type": "array"
                },
                {
                  "type": "string"
                }
              ]
            },
            "minItems": 1
          },
          "actions": {
            "type": "array",
            "items": {
              "type": "array",
              "minItems": 1
            }
          }
        },
        "required": ["actions"]
      }
    }
  },
  "required": ["rules"]
}
`

type Config struct {
	Rules []Rule `json:"rules,omitempty"`
}

type Rule struct {
	Case    []any    `json:"case,omitempty"`
	Actions []Action `json:"actions,omitempty"`
	expr    *pluginexpr.Expression
}

type Action struct {
	Name       string
	Config     map[string]any
	Return     ReturnAction
	limitConn  *limit_conn.Plugin
	limitCount *limit_count.Plugin
	limitReq   *limit_req.Plugin
}

type ReturnAction struct {
	Code int `json:"code,omitempty"`
}

func (a *Action) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw) == 0 {
		return fmt.Errorf("workflow action must contain a name")
	}
	if err := json.Unmarshal(raw[0], &a.Name); err != nil {
		return err
	}
	if len(raw) > 1 {
		if err := json.Unmarshal(raw[1], &a.Config); err != nil {
			return err
		}
	}
	if a.Name == "return" && len(raw) > 1 {
		return util.Parse(a.Config, &a.Return)
	}
	return nil
}

func (p *Plugin) Config() any {
	if config := p.publicConfig.Load(); config != nil {
		return config
	}
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) SetPluginEnabledChecker(checker func(string) bool) {
	p.enabledChecker = checker
}

func (p *Plugin) ValidatePreMaterialization() error {
	p.lifecycleMu.Lock()
	config := p.sourceConfigCloneLocked()
	p.lifecycleMu.Unlock()
	return p.validatePreMaterialization(config)
}

func (p *Plugin) validatePreMaterialization(config Config) error {
	for ruleIndex := range config.Rules {
		for actionIndex := range config.Rules[ruleIndex].Actions {
			action := &config.Rules[ruleIndex].Actions[actionIndex]
			switch action.Name {
			case "limit-req", "limit-conn", "limit-count":
				if p.enabledChecker != nil && !p.enabledChecker(action.Name) {
					return fmt.Errorf("workflow action plugin %q is disabled", action.Name)
				}
				if action.Name == "limit-count" {
					if _, ok := action.Config["group"]; ok {
						return fmt.Errorf("workflow rule %d limit-count action group is not supported", ruleIndex)
					}
				}
				child, err := newWorkflowChild(action.Name)
				if err != nil {
					return err
				}
				if err := child.Init(); err != nil {
					return err
				}
				compiledSchema, err := util.CompileSchema(child.GetSchema())
				if err != nil {
					return fmt.Errorf(
						"workflow rule %d action %d validation failed",
						ruleIndex,
						actionIndex,
					)
				}
				if err := compiledSchema.Validate(action.Config); err != nil {
					return fmt.Errorf(
						"workflow rule %d %s action validation failed: %w",
						ruleIndex,
						action.Name,
						err,
					)
				}
			case "return":
				if action.Return.Code < http.StatusOK || action.Return.Code > 599 {
					return fmt.Errorf(
						"workflow return action code must be between %d and 599",
						http.StatusOK,
					)
				}
			default:
				return fmt.Errorf("unsupported workflow action %q", action.Name)
			}
		}
	}
	return nil
}

func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context,
	access base.ScopedSecretAccess,
) error {
	if ctx == nil {
		return errWorkflowChildPreparation
	}
	if err := ctx.Err(); err != nil {
		return workflowChildPreparationError(ctx, err)
	}
	sourceConfig, token := p.beginPreparation()
	defer p.finishPreparation(token)
	if err := p.validatePreMaterialization(sourceConfig); err != nil {
		return err
	}
	preparer := p.CompositeChildPreparer()
	children := make(map[actionPosition]workflowChild)
	var owners []base.PreparedCompositeChild
	configs := make(map[actionPosition]map[string]any)
	committed := false
	defer func() {
		if !committed {
			closeWorkflowChildren(owners)
		}
	}()
	for ruleIndex := range sourceConfig.Rules {
		for actionIndex := range sourceConfig.Rules[ruleIndex].Actions {
			action := &sourceConfig.Rules[ruleIndex].Actions[actionIndex]
			if !isWorkflowLimitAction(action.Name) {
				continue
			}
			if preparer == nil {
				return errWorkflowChildPreparation
			}
			position := actionPosition{rule: ruleIndex, action: actionIndex}
			prepared, err := preparer.Prepare(ctx, access, base.CompositeChildSpec{
				Factory:  action.Name,
				Config:   cloneWorkflowConfig(action.Config),
				Position: workflowChildPosition(ruleIndex, actionIndex),
			})
			if prepared != nil {
				owners = append(owners, prepared)
			}
			if err != nil {
				return workflowChildPreparationError(ctx, err)
			}
			if prepared == nil {
				return errWorkflowChildPreparation
			}
			child, ok := preparedWorkflowChild(action.Name, prepared)
			if !ok {
				return errWorkflowChildPreparation
			}
			config, err := descriptorSafeActionConfig(action.Config, child.Config())
			if err != nil {
				return errWorkflowChildPreparation
			}
			children[position] = child
			configs[position] = config
		}
	}
	generation, publicConfig := prepareWorkflowGeneration(sourceConfig, children, nil, owners, configs)
	if !p.publishPreparedGeneration(ctx, true, token, generation, publicConfig) {
		return workflowChildPreparationError(ctx, errWorkflowChildPreparation)
	}
	committed = true
	return nil
}

func (p *Plugin) SetResourceContext(route resource.Route, service resource.Service) {
	p.lifecycleMu.Lock()
	p.route = route
	p.service = service
	p.resourceSet = true
	p.resourceEpoch++
	generation := p.currentGenerationLocked()
	if generation == nil || generation.retired || generation.active != 0 || generation.contextUpdating {
		p.lifecycleMu.Unlock()
		return
	}
	generation.contextUpdating = true
	generation.contextReady = make(chan struct{})
	generation.active++
	p.lifecycleMu.Unlock()

	defer p.finishContextUpdate(generation)
	for _, child := range generation.children {
		applyResourceContextToChild(child, route, service)
	}
}

func applyResourceContextToChild(child workflowChild, route resource.Route, service resource.Service) {
	if setter, ok := child.(interface {
		SetResourceContext(resource.Route, resource.Service)
	}); ok {
		setter.SetResourceContext(route, service)
	}
}

func descriptorSafeActionConfig(original map[string]any, materialized any) (map[string]any, error) {
	encoded, err := json.Marshal(materialized)
	if err != nil {
		return nil, err
	}
	var redacted map[string]any
	if err := json.Unmarshal(encoded, &redacted); err != nil {
		return nil, err
	}
	config := cloneWorkflowConfig(original)
	syncActionConfig(config, redacted)
	return config, nil
}

func syncActionConfig(config map[string]any, materialized map[string]any) {
	for key, value := range config {
		materializedValue, ok := materialized[key]
		if !ok {
			continue
		}
		configMap, configIsMap := value.(map[string]any)
		materializedMap, materializedIsMap := materializedValue.(map[string]any)
		if configIsMap && materializedIsMap {
			syncActionConfig(configMap, materializedMap)
			continue
		}
		config[key] = materializedValue
	}
}

func cloneWorkflowConfig(config map[string]any) map[string]any {
	cloned := make(map[string]any, len(config))
	for key, value := range config {
		cloned[key] = cloneWorkflowValue(value)
	}
	return cloned
}

func cloneWorkflowValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneWorkflowConfig(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneWorkflowValue(item)
		}
		return cloned
	default:
		return value
	}
}

func isWorkflowLimitAction(factory string) bool {
	switch factory {
	case "limit-req", "limit-conn", "limit-count":
		return true
	default:
		return false
	}
}

func newWorkflowChild(factory string) (workflowChild, error) {
	var child workflowChild
	switch factory {
	case "limit-req":
		child = &limit_req.Plugin{}
	case "limit-conn":
		child = &limit_conn.Plugin{}
	case "limit-count":
		child = &limit_count.Plugin{}
	default:
		return nil, fmt.Errorf("unsupported workflow action %q", factory)
	}
	return child, nil
}

func preparedWorkflowChild(
	factory string,
	prepared base.PreparedCompositeChild,
) (workflowChild, bool) {
	if prepared.Factory() != factory {
		return nil, false
	}
	switch factory {
	case "limit-req":
		child, ok := prepared.Instance().(*limit_req.Plugin)
		return child, ok && child != nil
	case "limit-conn":
		child, ok := prepared.Instance().(*limit_conn.Plugin)
		return child, ok && child != nil
	case "limit-count":
		child, ok := prepared.Instance().(*limit_count.Plugin)
		return child, ok && child != nil
	default:
		return nil, false
	}
}

func workflowChildPosition(rule, action int) string {
	return "workflow/rule/" + strconv.Itoa(rule) + "/action/" + strconv.Itoa(action)
}

func prepareWorkflowGeneration(
	sourceConfig Config,
	children map[actionPosition]workflowChild,
	stoppers []workflowChildStopper,
	owners []base.PreparedCompositeChild,
	configs map[actionPosition]map[string]any,
) (*workflowGeneration, Config) {
	runtimeConfig := cloneWorkflowPublicConfig(sourceConfig)
	publicConfig := cloneWorkflowPublicConfig(sourceConfig)
	for ruleIndex := range runtimeConfig.Rules {
		for actionIndex := range runtimeConfig.Rules[ruleIndex].Actions {
			action := &runtimeConfig.Rules[ruleIndex].Actions[actionIndex]
			position := actionPosition{rule: ruleIndex, action: actionIndex}
			if config, ok := configs[position]; ok {
				action.Config = cloneWorkflowConfig(config)
				publicConfig.Rules[ruleIndex].Actions[actionIndex].Config = cloneWorkflowConfig(config)
			}
			assignWorkflowActionChild(action, children[position])
		}
	}
	return &workflowGeneration{
		config: runtimeConfig, children: children, stoppers: stoppers, owners: owners,
	}, publicConfig
}

func (p *Plugin) beginPreparation() (Config, uint64) {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.preparationEpoch++
	p.preparingToken = p.preparationEpoch
	return p.sourceConfigCloneLocked(), p.preparingToken
}

func (p *Plugin) finishPreparation(token uint64) {
	p.lifecycleMu.Lock()
	if p.preparingToken == token {
		p.preparingToken = 0
	}
	p.lifecycleMu.Unlock()
}

func (p *Plugin) publishPreparedGeneration(
	ctx context.Context,
	checkCancellation bool,
	token uint64,
	generation *workflowGeneration,
	publicConfig Config,
) bool {
	for {
		p.lifecycleMu.Lock()
		if checkCancellation {
			if err := ctx.Err(); err != nil {
				p.lifecycleMu.Unlock()
				return false
			}
		}
		if p.preparingToken != token || p.preparationEpoch != token {
			p.lifecycleMu.Unlock()
			return false
		}
		resourceSet := p.resourceSet
		route := p.route
		service := p.service
		resourceEpoch := p.resourceEpoch
		p.lifecycleMu.Unlock()

		if resourceSet {
			for _, child := range generation.children {
				applyResourceContextToChild(child, route, service)
			}
		}

		p.lifecycleMu.Lock()
		if checkCancellation {
			if err := ctx.Err(); err != nil {
				p.lifecycleMu.Unlock()
				return false
			}
		}
		if p.preparingToken != token || p.preparationEpoch != token {
			p.lifecycleMu.Unlock()
			return false
		}
		if p.resourceEpoch != resourceEpoch {
			p.lifecycleMu.Unlock()
			continue
		}
		old := p.currentGenerationLocked()
		p.current = generation
		p.config = generation.config
		p.children = generation.children
		p.childStoppers = generation.stoppers
		p.childOwners = generation.owners
		p.publicConfig.Store(&publicConfig)
		p.preparingToken = 0
		retirement := p.retireGenerationLocked(old)
		p.lifecycleMu.Unlock()

		closeWorkflowRetirement(retirement)
		return true
	}
}

func (p *Plugin) sourceConfigCloneLocked() Config {
	if !p.sourceConfigSet {
		p.sourceConfig = cloneWorkflowPublicConfig(p.config)
		p.sourceConfigSet = true
	}
	return cloneWorkflowPublicConfig(p.sourceConfig)
}

func cloneWorkflowPublicConfig(config Config) Config {
	cloned := Config{Rules: make([]Rule, len(config.Rules))}
	for ruleIndex := range config.Rules {
		rule := config.Rules[ruleIndex]
		cloned.Rules[ruleIndex].Case = make([]any, len(rule.Case))
		for index, value := range rule.Case {
			cloned.Rules[ruleIndex].Case[index] = cloneWorkflowValue(value)
		}
		cloned.Rules[ruleIndex].Actions = make([]Action, len(rule.Actions))
		for actionIndex := range rule.Actions {
			action := rule.Actions[actionIndex]
			cloned.Rules[ruleIndex].Actions[actionIndex] = Action{
				Name: action.Name, Config: cloneWorkflowConfig(action.Config), Return: action.Return,
			}
		}
	}
	return cloned
}

func cloneWorkflowRuntimeConfig(config Config) Config {
	cloned := cloneWorkflowPublicConfig(config)
	for ruleIndex := range config.Rules {
		cloned.Rules[ruleIndex].expr = config.Rules[ruleIndex].expr
		for actionIndex := range config.Rules[ruleIndex].Actions {
			source := config.Rules[ruleIndex].Actions[actionIndex]
			target := &cloned.Rules[ruleIndex].Actions[actionIndex]
			target.limitReq = source.limitReq
			target.limitConn = source.limitConn
			target.limitCount = source.limitCount
		}
	}
	return cloned
}

func assignWorkflowActionChild(action *Action, child workflowChild) {
	action.limitReq = nil
	action.limitConn = nil
	action.limitCount = nil
	switch action.Name {
	case "limit-req":
		action.limitReq, _ = child.(*limit_req.Plugin)
	case "limit-conn":
		action.limitConn, _ = child.(*limit_conn.Plugin)
	case "limit-count":
		action.limitCount, _ = child.(*limit_count.Plugin)
	}
}

func (p *Plugin) PostInit() error {
	p.lifecycleMu.Lock()
	if p.preparingToken != 0 {
		p.lifecycleMu.Unlock()
		return errWorkflowLifecycleBusy
	}
	generation := p.currentGenerationLocked()
	if generation != nil && generation.active != 0 {
		p.lifecycleMu.Unlock()
		return errWorkflowLifecycleBusy
	}
	sourceConfig := p.sourceConfigCloneLocked()
	runtimeConfig := cloneWorkflowRuntimeConfig(p.config)
	if generation != nil {
		runtimeConfig = cloneWorkflowRuntimeConfig(generation.config)
	}
	p.lifecycleMu.Unlock()

	if err := p.validatePreMaterialization(sourceConfig); err != nil {
		p.retireGenerationAfterPostInitFailure(generation)
		return err
	}
	for ruleIndex := range runtimeConfig.Rules {
		rule := &runtimeConfig.Rules[ruleIndex]
		if len(rule.Case) > 0 {
			expr, err := pluginexpr.Compile(rule.Case)
			if err != nil {
				p.retireGenerationAfterPostInitFailure(generation)
				return fmt.Errorf("workflow rule %d case validation failed: %w", ruleIndex, err)
			}
			rule.expr = expr
		}
		for actionIndex := range runtimeConfig.Rules[ruleIndex].Actions {
			action := &runtimeConfig.Rules[ruleIndex].Actions[actionIndex]
			switch action.Name {
			case "limit-req":
				if action.limitReq == nil {
					p.retireGenerationAfterPostInitFailure(generation)
					return fmt.Errorf("workflow action plugin %q was not materialized", action.Name)
				}
			case "limit-conn":
				if action.limitConn == nil {
					p.retireGenerationAfterPostInitFailure(generation)
					return fmt.Errorf("workflow action plugin %q was not materialized", action.Name)
				}
			case "limit-count":
				if action.limitCount == nil {
					p.retireGenerationAfterPostInitFailure(generation)
					return fmt.Errorf("workflow action plugin %q was not materialized", action.Name)
				}
			}
		}
	}

	p.lifecycleMu.Lock()
	if p.preparingToken != 0 || p.currentGenerationLocked() != generation ||
		(generation != nil && (generation.retired || generation.active != 0)) {
		p.lifecycleMu.Unlock()
		return errWorkflowLifecycleBusy
	}
	if generation != nil {
		generation.config = runtimeConfig
	}
	p.config = runtimeConfig
	p.lifecycleMu.Unlock()
	return nil
}

func (p *Plugin) retireGenerationAfterPostInitFailure(generation *workflowGeneration) {
	p.lifecycleMu.Lock()
	var retirement workflowRetirement
	if p.preparingToken == 0 && p.currentGenerationLocked() == generation {
		p.current = nil
		p.clearPublishedChildrenLocked()
		retirement = p.retireGenerationLocked(generation)
	}
	p.lifecycleMu.Unlock()
	closeWorkflowRetirement(retirement)
}

func (p *Plugin) currentGenerationLocked() *workflowGeneration {
	if p.current == nil && (p.children != nil || p.childStoppers != nil || p.childOwners != nil) {
		p.current = &workflowGeneration{
			config: p.config, children: p.children, stoppers: p.childStoppers, owners: p.childOwners,
		}
	}
	return p.current
}

func (p *Plugin) retireGenerationLocked(generation *workflowGeneration) workflowRetirement {
	if generation == nil {
		return workflowRetirement{}
	}
	generation.retired = true
	if !generation.contextUpdating {
		signalWorkflowContextReadyLocked(generation)
	}
	return p.scheduleGenerationCloseLocked(generation)
}

func (p *Plugin) scheduleGenerationCloseLocked(generation *workflowGeneration) workflowRetirement {
	if !generation.retired || generation.active != 0 || generation.closeScheduled {
		return workflowRetirement{}
	}
	generation.closeScheduled = true
	retirement := workflowRetirement{stoppers: generation.stoppers, owners: generation.owners}
	generation.stoppers = nil
	generation.owners = nil
	return retirement
}

func (p *Plugin) releaseGeneration(generation *workflowGeneration) {
	p.lifecycleMu.Lock()
	generation.active--
	retirement := p.scheduleGenerationCloseLocked(generation)
	p.lifecycleMu.Unlock()
	closeWorkflowRetirement(retirement)
}

func (p *Plugin) finishContextUpdate(generation *workflowGeneration) {
	p.lifecycleMu.Lock()
	generation.contextUpdating = false
	signalWorkflowContextReadyLocked(generation)
	generation.active--
	retirement := p.scheduleGenerationCloseLocked(generation)
	p.lifecycleMu.Unlock()
	closeWorkflowRetirement(retirement)
}

func signalWorkflowContextReadyLocked(generation *workflowGeneration) {
	if generation.contextReady == nil {
		return
	}
	close(generation.contextReady)
	generation.contextReady = nil
}

func (p *Plugin) clearPublishedChildrenLocked() {
	p.childStoppers = nil
	p.childOwners = nil
	p.children = nil
	p.config = cloneWorkflowRuntimeConfig(p.config)
	for ruleIndex := range p.config.Rules {
		for actionIndex := range p.config.Rules[ruleIndex].Actions {
			assignWorkflowActionChild(&p.config.Rules[ruleIndex].Actions[actionIndex], nil)
		}
	}
}

func closeWorkflowChildren(children []base.PreparedCompositeChild) {
	for _, child := range slices.Backward(children) {
		child.Close()
	}
}

func stopWorkflowChildren(children []workflowChildStopper) {
	for _, child := range slices.Backward(children) {
		child.Stop()
	}
}

func closeWorkflowRetirement(retirement workflowRetirement) {
	closeWorkflowChildren(retirement.owners)
	stopWorkflowChildren(retirement.stoppers)
}

func workflowChildPreparationError(ctx context.Context, err error) error {
	if ctx != nil {
		switch ctx.Err() {
		case context.Canceled:
			return context.Canceled
		case context.DeadlineExceeded:
			return context.DeadlineExceeded
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errWorkflowChildPreparation
}

func (p *Plugin) Stop() {
	p.lifecycleMu.Lock()
	p.preparationEpoch++
	p.preparingToken = 0
	generation := p.currentGenerationLocked()
	p.current = nil
	p.clearPublishedChildrenLocked()
	retirement := p.retireGenerationLocked(generation)
	p.lifecycleMu.Unlock()
	closeWorkflowRetirement(retirement)
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if !p.handleRequestWithLease(w, r, next) {
			next.ServeHTTP(w, r)
		}
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) handleRequestWithLease(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
) bool {
	var generation *workflowGeneration
	var config Config
	for {
		p.lifecycleMu.Lock()
		generation = p.currentGenerationLocked()
		if generation == nil || generation.retired {
			p.lifecycleMu.Unlock()
			return false
		}
		if generation.contextUpdating {
			ready := generation.contextReady
			p.lifecycleMu.Unlock()
			<-ready
			continue
		}
		generation.active++
		config = generation.config
		p.lifecycleMu.Unlock()
		break
	}
	defer p.releaseGeneration(generation)

	// One resolver per request: rule matching never allocates a closure
	// per step, so scanning hundreds of rules stays allocation-free.
	resolve := func(name string) any {
		return pluginexpr.RequestValue(r, name)
	}
	for _, rule := range config.Rules {
		if !matchRule(r, rule, resolve) {
			continue
		}
		return p.handleAction(w, r, next, rule.Actions)
	}
	return false
}

func (p *Plugin) handleAction(w http.ResponseWriter, r *http.Request, next http.Handler, actions []Action) bool {
	if len(actions) == 0 {
		return false
	}
	action := actions[0]
	if action.Name == "limit-req" && action.limitReq != nil {
		r = p.withConsumerActionOverride(r, action.Name)
		action.limitReq.Handler(next).ServeHTTP(w, r)
		return true
	}
	if action.Name == "limit-conn" && action.limitConn != nil {
		r = p.withConsumerActionOverride(r, action.Name)
		action.limitConn.Handler(next).ServeHTTP(w, r)
		return true
	}
	if action.Name == "limit-count" && action.limitCount != nil {
		r = p.withConsumerActionOverride(r, action.Name)
		action.limitCount.Handler(next).ServeHTTP(w, r)
		return true
	}

	if action.Name != "return" {
		return false
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(action.Return.Code)
	_, _ = fmt.Fprintln(w, `{"error_msg":"rejected by workflow"}`)
	return true
}

func (p *Plugin) withConsumerActionOverride(r *http.Request, actionName string) *http.Request {
	if !apisixctx.ConsumerPluginOverrides(r, name) {
		return r
	}
	consumer, ok := apisixctx.GetApisixVar(r, "$consumer").(resource.Consumer)
	if !ok {
		return r
	}

	overrides := make(map[string]struct{}, len(consumer.Plugins)+1)
	if consumer.GroupID != "" {
		if lookup := p.ConsumerLookup(); lookup != nil {
			if group, found := lookup.ConsumerGroupByID(consumer.GroupID); found {
				for pluginName := range group.Plugins {
					overrides[pluginName] = struct{}{}
				}
			}
		}
	}
	for pluginName := range consumer.Plugins {
		overrides[pluginName] = struct{}{}
	}
	overrides[actionName] = struct{}{}
	return apisixctx.WithConsumerPluginOverrides(r, overrides)
}

func matchRule(r *http.Request, rule Rule, resolve pluginexpr.Resolver) bool {
	if len(rule.Case) == 0 {
		return true
	}
	if rule.expr == nil {
		return false
	}
	return rule.expr.Eval(resolve)
}
