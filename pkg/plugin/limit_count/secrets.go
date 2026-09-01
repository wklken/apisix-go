package limit_count

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
)

var errLimitCountCredentialsUnavailable = secret.ErrCredentialUnavailable

type secretFieldSelection struct {
	field string
	raw   string
}

type limitCountSecretState struct {
	lifecycleMu sync.Mutex

	preparationMu      sync.Mutex
	preparationCond    *sync.Cond
	preparationActive  bool
	preparationWaiters int

	credentialMu sync.Mutex
	stopOnce     sync.Once

	scopedKeySecret         secret.Value
	scopedRedisHost         secret.Value
	scopedRedisClusterNodes []secret.Value

	keyPresent        bool
	redisHostPresent  bool
	redisNodesPresent bool
	scopedSet         bool

	keyField        string
	redisHostField  string
	redisNodesField string

	activeUses int
	usesDone   chan struct{}
	retired    bool
}

type limitCountSecretSnapshot struct {
	scopedKey               secret.Value
	scopedRedisHost         secret.Value
	scopedRedisClusterNodes []secret.Value

	keyPresent        bool
	redisHostPresent  bool
	redisNodesPresent bool
}

type stagedLimitCountSecrets struct {
	keySelection   secretFieldSelection
	hostSelection  secretFieldSelection
	nodesSelection secretFieldSelection

	scopedKey   secret.Value
	scopedHost  secret.Value
	scopedNodes []secret.Value

	keyDescriptor   string
	hostDescriptor  string
	nodeDescriptors []string
}

func selectLimitCountSecretFields(config Config) (
	secretFieldSelection, secretFieldSelection, secretFieldSelection,
) {
	key := secretFieldSelection{}
	if config.Key != "" {
		key = secretFieldSelection{field: "key", raw: config.Key}
	}
	host := secretFieldSelection{}
	if config.RedisHost != "" {
		host = secretFieldSelection{field: "redis_host", raw: config.RedisHost}
	}
	nodes := secretFieldSelection{}
	if len(config.RedisClusterNodes) > 0 {
		nodes = secretFieldSelection{
			field: "redis_cluster_nodes",
			raw:   strings.Join(config.RedisClusterNodes, "\x00"),
		}
	}
	return key, host, nodes
}

// MaterializeScopedSecrets resolves the catalog-declared fields before plugin
// initialization replaces their public values with content descriptors.
func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context, access base.ScopedSecretAccess,
) error {
	p.beginLimitCountPreparation()
	defer p.endLimitCountPreparation()
	if prepared, err := p.limitCountPreparationState(); err != nil || prepared {
		return err
	}
	keySelection, hostSelection, nodesSelection := selectLimitCountSecretFields(p.config)
	nodes := append([]string(nil), p.config.RedisClusterNodes...)
	staged := stagedLimitCountSecrets{
		keySelection: keySelection, hostSelection: hostSelection, nodesSelection: nodesSelection,
	}
	var err error
	if keySelection.raw != "" {
		staged.scopedKey, err = access.Materialize(ctx, keySelection.field, keySelection.raw)
		if err != nil || !validLimitCountScopedValue(staged.scopedKey) {
			return errors.New("resolve limit-count key: credential unavailable")
		}
		staged.keyDescriptor, err = scopedLimitCountDescriptor(staged.scopedKey)
		if err != nil {
			return errLimitCountCredentialsUnavailable
		}
	}
	if hostSelection.raw != "" {
		staged.scopedHost, err = access.Materialize(ctx, hostSelection.field, hostSelection.raw)
		if err != nil || !validLimitCountScopedValue(staged.scopedHost) {
			return errors.New("resolve limit-count Redis host: credential unavailable")
		}
		staged.hostDescriptor, err = scopedLimitCountDescriptor(staged.scopedHost)
		if err != nil {
			return errLimitCountCredentialsUnavailable
		}
	}
	staged.scopedNodes = make([]secret.Value, len(nodes))
	staged.nodeDescriptors = make([]string, len(nodes))
	for index, raw := range nodes {
		staged.scopedNodes[index], err = access.Materialize(ctx, nodesSelection.field, raw)
		if err != nil || !validLimitCountScopedValue(staged.scopedNodes[index]) {
			return fmt.Errorf("resolve limit-count Redis cluster node %d: credential unavailable", index)
		}
		staged.nodeDescriptors[index], err = scopedLimitCountDescriptor(
			staged.scopedNodes[index],
		)
		if err != nil {
			return fmt.Errorf("resolve limit-count Redis cluster node %d: credential unavailable", index)
		}
	}
	return p.installLimitCountSecrets(staged)
}

// PrepareConsumerConfig admits a consumer-scoped configuration that has no
// catalog-declared secret fields. Such a configuration keeps its literal
// values in the plugin config and still enters the same credential lifecycle
// as a scoped materialization. Secret envelopes are rejected instead of being
// allowed to bypass generation-scoped materialization.
func (p *Plugin) PrepareConsumerConfig() error {
	p.beginLimitCountPreparation()
	defer p.endLimitCountPreparation()
	if prepared, err := p.limitCountPreparationState(); err != nil || prepared {
		return err
	}
	if limitCountConfigHasUnmaterializedSecret(p.config) {
		return errLimitCountCredentialsUnavailable
	}
	return p.installLimitCountLiteralSecrets()
}

func (p *Plugin) beginLimitCountPreparation() {
	p.preparationMu.Lock()
	if p.preparationCond == nil {
		p.preparationCond = sync.NewCond(&p.preparationMu)
	}
	if p.preparationActive {
		p.preparationWaiters++
		for p.preparationActive {
			p.preparationCond.Wait()
		}
		p.preparationWaiters--
	}
	p.preparationActive = true
	p.preparationMu.Unlock()
}

func (p *Plugin) endLimitCountPreparation() {
	p.preparationMu.Lock()
	p.preparationActive = false
	p.preparationCond.Broadcast()
	p.preparationMu.Unlock()
}

func (p *Plugin) limitCountPreparationState() (bool, error) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired {
		return false, errLimitCountCredentialsUnavailable
	}
	return p.scopedSet, nil
}

func (p *Plugin) installLimitCountSecrets(staged stagedLimitCountSecrets) error {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired {
		return errLimitCountCredentialsUnavailable
	}
	p.scopedKeySecret = staged.scopedKey
	p.scopedRedisHost = staged.scopedHost
	p.scopedRedisClusterNodes = staged.scopedNodes
	p.keyPresent = staged.keySelection.raw != ""
	p.redisHostPresent = staged.hostSelection.raw != ""
	p.redisNodesPresent = staged.nodesSelection.field != ""
	p.scopedSet = true
	p.keyField = staged.keySelection.field
	p.redisHostField = staged.hostSelection.field
	p.redisNodesField = staged.nodesSelection.field

	if staged.keyDescriptor != "" {
		p.config.Key = staged.keyDescriptor
	}
	if staged.hostDescriptor != "" {
		p.config.RedisHost = staged.hostDescriptor
	}
	if len(staged.nodeDescriptors) > 0 {
		p.config.RedisClusterNodes = append([]string(nil), staged.nodeDescriptors...)
	}
	return nil
}

func (p *Plugin) installLimitCountLiteralSecrets() error {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired {
		return errLimitCountCredentialsUnavailable
	}
	p.scopedKeySecret = secret.Value{}
	p.scopedRedisHost = secret.Value{}
	p.scopedRedisClusterNodes = nil
	p.keyPresent = false
	p.redisHostPresent = false
	p.redisNodesPresent = false
	p.scopedSet = true
	p.keyField = ""
	p.redisHostField = ""
	p.redisNodesField = ""
	return nil
}

func limitCountConfigHasUnmaterializedSecret(config Config) bool {
	return limitCountValueHasUnmaterializedSecret(reflect.ValueOf(config), 0, make(map[uintptr]struct{}))
}

func limitCountValueHasUnmaterializedSecret(
	value reflect.Value, depth int, visited map[uintptr]struct{},
) bool {
	if !value.IsValid() {
		return false
	}
	if depth >= 32 {
		return true
	}
	for value.Kind() == reflect.Interface {
		if value.IsNil() {
			return false
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return false
		}
		pointer := value.Pointer()
		if _, exists := visited[pointer]; exists {
			return false
		}
		visited[pointer] = struct{}{}
		defer delete(visited, pointer)
		return limitCountValueHasUnmaterializedSecret(value.Elem(), depth+1, visited)
	case reflect.String:
		return isUnmaterializedLimitCountSecret(value.String())
	case reflect.Struct:
		for _, field := range value.Fields() {
			if limitCountValueHasUnmaterializedSecret(field, depth+1, visited) {
				return true
			}
		}
	case reflect.Map:
		for _, key := range value.MapKeys() {
			if limitCountValueHasUnmaterializedSecret(value.MapIndex(key), depth+1, visited) {
				return true
			}
		}
	case reflect.Array, reflect.Slice:
		for index := 0; index < value.Len(); index++ {
			if limitCountValueHasUnmaterializedSecret(value.Index(index), depth+1, visited) {
				return true
			}
		}
	}
	return false
}

func isUnmaterializedLimitCountSecret(value string) bool {
	if len(value) >= len("$ENV://") && strings.EqualFold(value[:len("$ENV://")], "$ENV://") {
		return !strings.Contains(value, "#sha256:")
	}
	if strings.HasPrefix(value, "$secret://") || strings.HasPrefix(value, "$encrypted://") {
		return !strings.Contains(value, "#sha256:")
	}
	return false
}

func validLimitCountScopedValue(value secret.Value) bool {
	valid := false
	_ = value.Use(func(plaintext string) error {
		valid = strings.TrimSpace(plaintext) != ""
		return nil
	})
	return valid
}

func scopedLimitCountDescriptor(
	value secret.Value,
) (string, error) {
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return "", err
	}
	return descriptor.String(), nil
}

func (p *Plugin) acquireLimitCountSecrets() (limitCountSecretSnapshot, func(), error) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired || !p.scopedSet {
		return limitCountSecretSnapshot{}, nil, errLimitCountCredentialsUnavailable
	}
	if p.activeUses == 0 {
		p.usesDone = make(chan struct{})
	}
	p.activeUses++
	return limitCountSecretSnapshot{
		scopedKey: p.scopedKeySecret, scopedRedisHost: p.scopedRedisHost,
		scopedRedisClusterNodes: append([]secret.Value(nil), p.scopedRedisClusterNodes...),
		keyPresent:              p.keyPresent, redisHostPresent: p.redisHostPresent,
		redisNodesPresent: p.redisNodesPresent,
	}, p.releaseLimitCountSecretUse, nil
}

func (p *Plugin) releaseLimitCountSecretUse() {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	p.activeUses--
	if p.activeUses == 0 {
		close(p.usesDone)
		p.usesDone = nil
	}
}

func (p *Plugin) withLimitCountKey(use func(string) error) error {
	snapshot, release, err := p.acquireLimitCountSecrets()
	if err != nil {
		return err
	}
	defer release()
	if !snapshot.keyPresent {
		return use(p.config.Key)
	}
	return snapshot.scopedKey.Use(use)
}

func (p *Plugin) withLimitCountRedisHost(use func(string) error) error {
	snapshot, release, err := p.acquireLimitCountSecrets()
	if err != nil {
		return err
	}
	defer release()
	if !snapshot.redisHostPresent {
		return use(p.config.RedisHost)
	}
	return snapshot.scopedRedisHost.Use(use)
}

func (p *Plugin) withLimitCountRedisNodes(use func([]string) error) error {
	snapshot, release, err := p.acquireLimitCountSecrets()
	if err != nil {
		return err
	}
	defer release()
	if !snapshot.redisNodesPresent {
		return use(append([]string(nil), p.config.RedisClusterNodes...))
	}
	nodes := make([]string, len(snapshot.scopedRedisClusterNodes))
	var useNode func(int) error
	useNode = func(index int) error {
		if index == len(snapshot.scopedRedisClusterNodes) {
			return use(nodes)
		}
		return snapshot.scopedRedisClusterNodes[index].Use(func(plaintext string) error {
			nodes[index] = plaintext
			defer func() { nodes[index] = "" }()
			return useNode(index + 1)
		})
	}
	return useNode(0)
}
