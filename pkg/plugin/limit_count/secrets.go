package limit_count

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/store"
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

	keySecret               *store.ResolvedSecret
	redisHostSecret         *store.ResolvedSecret
	redisClusterNodeSecrets []*store.ResolvedSecret

	scopedKeySecret         secret.Value
	scopedRedisHost         secret.Value
	scopedRedisClusterNodes []secret.Value

	keyPresent        bool
	redisHostPresent  bool
	redisNodesPresent bool
	legacySet         bool
	scopedSet         bool

	legacyKey        string
	legacyRedisHost  string
	legacyRedisNodes []string

	keyDigest        [sha256.Size]byte
	redisHostDigest  [sha256.Size]byte
	redisNodeDigests [][sha256.Size]byte

	keyField        string
	redisHostField  string
	redisNodesField string

	activeUses int
	usesDone   chan struct{}
	retired    bool
}

type limitCountSecretSnapshot struct {
	legacy bool

	keySecret               *store.ResolvedSecret
	redisHostSecret         *store.ResolvedSecret
	redisClusterNodeSecrets []*store.ResolvedSecret

	scopedKey               secret.Value
	scopedRedisHost         secret.Value
	scopedRedisClusterNodes []secret.Value

	keyPresent        bool
	redisHostPresent  bool
	redisNodesPresent bool

	legacyKey        string
	legacyRedisHost  string
	legacyRedisNodes []string
}

type stagedLimitCountSecrets struct {
	legacy bool

	keySelection   secretFieldSelection
	hostSelection  secretFieldSelection
	nodesSelection secretFieldSelection

	keyOwner   *store.ResolvedSecret
	hostOwner  *store.ResolvedSecret
	nodeOwners []*store.ResolvedSecret

	scopedKey   secret.Value
	scopedHost  secret.Value
	scopedNodes []secret.Value

	keyDescriptor   string
	hostDescriptor  string
	nodeDescriptors []string

	keyDigest   [sha256.Size]byte
	hostDigest  [sha256.Size]byte
	nodeDigests [][sha256.Size]byte

	legacyKey   string
	legacyHost  string
	legacyNodes []string
}

func selectLimitCountSecretFields(config Config) (
	secretFieldSelection, secretFieldSelection, secretFieldSelection,
) {
	key := secretFieldSelection{}
	if config.Key != "" {
		key = secretFieldSelection{field: "key", raw: config.Key}
	}
	host := secretFieldSelection{}
	if config.Redis.RedisHost != "" {
		host = secretFieldSelection{field: "redis_config.redis_host", raw: config.Redis.RedisHost}
	} else if config.RedisHost != "" {
		host = secretFieldSelection{field: "redis_host", raw: config.RedisHost}
	}
	nodes := secretFieldSelection{}
	if len(config.RedisCluster.RedisClusterNodes) > 0 {
		nodes = secretFieldSelection{
			field: "redis_cluster_config.redis_cluster_nodes",
			raw:   strings.Join(config.RedisCluster.RedisClusterNodes, "\x00"),
		}
	} else if len(config.RedisClusterNodes) > 0 {
		nodes = secretFieldSelection{
			field: "redis_cluster_nodes",
			raw:   strings.Join(config.RedisClusterNodes, "\x00"),
		}
	}
	return key, host, nodes
}

func selectedLimitCountNodes(config Config, selection secretFieldSelection) []string {
	switch selection.field {
	case "redis_cluster_config.redis_cluster_nodes":
		return append([]string(nil), config.RedisCluster.RedisClusterNodes...)
	case "redis_cluster_nodes":
		return append([]string(nil), config.RedisClusterNodes...)
	default:
		return nil
	}
}

// MaterializeSecrets is the transitional Store-backed preparation path.
func (p *Plugin) MaterializeSecrets() error {
	p.beginLimitCountPreparation()
	defer p.endLimitCountPreparation()
	if prepared, err := p.limitCountPreparationState(); err != nil || prepared {
		return err
	}
	keySelection, hostSelection, nodesSelection := selectLimitCountSecretFields(p.config)
	nodes := selectedLimitCountNodes(p.config, nodesSelection)
	staged := stagedLimitCountSecrets{
		legacy: true, keySelection: keySelection, hostSelection: hostSelection,
		nodesSelection: nodesSelection, legacyKey: keySelection.raw,
		legacyHost: hostSelection.raw, legacyNodes: append([]string(nil), nodes...),
	}
	var err error
	if keySelection.raw != "" {
		staged.keyOwner, err = materializeLimitCountLegacyValue(keySelection.raw)
		if err != nil {
			return fmt.Errorf("resolve limit-count key: %w", errLimitCountCredentialsUnavailable)
		}
		staged.keyDigest, staged.keyDescriptor = legacyLimitCountDigest(staged.keyOwner, keySelection.raw)
	}
	if hostSelection.raw != "" {
		staged.hostOwner, err = materializeLimitCountLegacyValue(hostSelection.raw)
		if err != nil {
			staged.destroyLegacy()
			return fmt.Errorf("resolve limit-count Redis host: %w", errLimitCountCredentialsUnavailable)
		}
		staged.hostDigest, staged.hostDescriptor = legacyLimitCountDigest(staged.hostOwner, hostSelection.raw)
	}
	staged.nodeOwners = make([]*store.ResolvedSecret, len(nodes))
	staged.nodeDescriptors = make([]string, len(nodes))
	staged.nodeDigests = make([][sha256.Size]byte, len(nodes))
	for index, raw := range nodes {
		staged.nodeOwners[index], err = materializeLimitCountLegacyValue(raw)
		if err != nil {
			staged.destroyLegacy()
			return fmt.Errorf(
				"resolve limit-count Redis cluster node %d: %w",
				index,
				errLimitCountCredentialsUnavailable,
			)
		}
		staged.nodeDigests[index], staged.nodeDescriptors[index] = legacyLimitCountDigest(
			staged.nodeOwners[index], raw,
		)
		if staged.nodeDescriptors[index] == "" {
			staged.nodeDescriptors[index] = raw
		}
	}
	return p.installLimitCountSecrets(staged)
}

// MaterializeScopedSecrets resolves the selected aliases using their exact
// admitted manifest declarations before any root-to-nested normalization.
func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context, access base.ScopedSecretAccess,
) error {
	p.beginLimitCountPreparation()
	defer p.endLimitCountPreparation()
	if prepared, err := p.limitCountPreparationState(); err != nil || prepared {
		return err
	}
	keySelection, hostSelection, nodesSelection := selectLimitCountSecretFields(p.config)
	nodes := selectedLimitCountNodes(p.config, nodesSelection)
	staged := stagedLimitCountSecrets{
		keySelection: keySelection, hostSelection: hostSelection, nodesSelection: nodesSelection,
	}
	var err error
	if keySelection.raw != "" {
		staged.scopedKey, err = access.Materialize(ctx, keySelection.field, keySelection.raw)
		if err != nil || !validLimitCountScopedValue(staged.scopedKey) {
			return errors.New("resolve limit-count key: credential unavailable")
		}
		staged.keyDigest, staged.keyDescriptor, err = scopedLimitCountDescriptor(staged.scopedKey)
		if err != nil {
			return errLimitCountCredentialsUnavailable
		}
	}
	if hostSelection.raw != "" {
		staged.scopedHost, err = access.Materialize(ctx, hostSelection.field, hostSelection.raw)
		if err != nil || !validLimitCountScopedValue(staged.scopedHost) {
			return errors.New("resolve limit-count Redis host: credential unavailable")
		}
		staged.hostDigest, staged.hostDescriptor, err = scopedLimitCountDescriptor(staged.scopedHost)
		if err != nil {
			return errLimitCountCredentialsUnavailable
		}
	}
	staged.scopedNodes = make([]secret.Value, len(nodes))
	staged.nodeDescriptors = make([]string, len(nodes))
	staged.nodeDigests = make([][sha256.Size]byte, len(nodes))
	for index, raw := range nodes {
		staged.scopedNodes[index], err = access.Materialize(ctx, nodesSelection.field, raw)
		if err != nil || !validLimitCountScopedValue(staged.scopedNodes[index]) {
			return fmt.Errorf("resolve limit-count Redis cluster node %d: credential unavailable", index)
		}
		staged.nodeDigests[index], staged.nodeDescriptors[index], err = scopedLimitCountDescriptor(
			staged.scopedNodes[index],
		)
		if err != nil {
			return fmt.Errorf("resolve limit-count Redis cluster node %d: credential unavailable", index)
		}
	}
	return p.installLimitCountSecrets(staged)
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
	return p.legacySet || p.scopedSet, nil
}

func (p *Plugin) installLimitCountSecrets(staged stagedLimitCountSecrets) error {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired {
		staged.destroyLegacy()
		return errLimitCountCredentialsUnavailable
	}
	p.keySecret = staged.keyOwner
	p.redisHostSecret = staged.hostOwner
	p.redisClusterNodeSecrets = staged.nodeOwners
	p.scopedKeySecret = staged.scopedKey
	p.scopedRedisHost = staged.scopedHost
	p.scopedRedisClusterNodes = staged.scopedNodes
	p.keyPresent = staged.keySelection.raw != ""
	p.redisHostPresent = staged.hostSelection.raw != ""
	p.redisNodesPresent = staged.nodesSelection.field != ""
	p.legacySet = staged.legacy
	p.scopedSet = !staged.legacy
	p.legacyKey = staged.legacyKey
	p.legacyRedisHost = staged.legacyHost
	p.legacyRedisNodes = staged.legacyNodes
	p.keyDigest = staged.keyDigest
	p.redisHostDigest = staged.hostDigest
	p.redisNodeDigests = staged.nodeDigests
	p.keyField = staged.keySelection.field
	p.redisHostField = staged.hostSelection.field
	p.redisNodesField = staged.nodesSelection.field

	if staged.keyDescriptor != "" {
		p.config.Key = staged.keyDescriptor
	}
	if staged.hostDescriptor != "" {
		if staged.hostSelection.field == "redis_host" {
			p.config.RedisHost = staged.hostDescriptor
			p.applyRootRedisConfig()
		} else {
			p.config.Redis.RedisHost = staged.hostDescriptor
			if p.config.RedisHost != "" {
				p.config.RedisHost = staged.hostDescriptor
			}
		}
	}
	if len(staged.nodeDescriptors) > 0 {
		if staged.nodesSelection.field == "redis_cluster_nodes" {
			p.config.RedisClusterNodes = append([]string(nil), staged.nodeDescriptors...)
			p.applyRootRedisClusterConfig()
		} else {
			p.config.RedisCluster.RedisClusterNodes = append([]string(nil), staged.nodeDescriptors...)
			if len(p.config.RedisClusterNodes) > 0 {
				p.config.RedisClusterNodes = append([]string(nil), staged.nodeDescriptors...)
			}
		}
	}
	return nil
}

func materializeLimitCountLegacyValue(raw string) (*store.ResolvedSecret, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errLimitCountCredentialsUnavailable
	}
	value, err := store.MaterializeSecret(raw)
	if err != nil || !validLimitCountLegacyValue(value) {
		value.Destroy()
		return nil, errLimitCountCredentialsUnavailable
	}
	return value, nil
}

func validLimitCountLegacyValue(value *store.ResolvedSecret) bool {
	if value == nil {
		return false
	}
	plaintext := value.Bytes()
	defer clear(plaintext)
	return strings.TrimSpace(string(plaintext)) != ""
}

func validLimitCountScopedValue(value secret.Value) bool {
	valid := false
	_ = value.Use(func(plaintext string) error {
		valid = strings.TrimSpace(plaintext) != ""
		return nil
	})
	return valid
}

func legacyLimitCountDigest(
	owner *store.ResolvedSecret, literal string,
) ([sha256.Size]byte, string) {
	if owner == nil {
		return sha256.Sum256([]byte(literal)), ""
	}
	plaintext := owner.Bytes()
	defer clear(plaintext)
	digest := sha256.Sum256(plaintext)
	descriptor, _ := secret.NewDescriptor(capability.SecretPluginConfig, digest)
	return digest, descriptor.String()
}

func scopedLimitCountDescriptor(
	value secret.Value,
) ([sha256.Size]byte, string, error) {
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return [sha256.Size]byte{}, "", err
	}
	return descriptor.Digest(), descriptor.String(), nil
}

func (staged *stagedLimitCountSecrets) destroyLegacy() {
	staged.keyOwner.Destroy()
	staged.hostOwner.Destroy()
	for _, owner := range staged.nodeOwners {
		owner.Destroy()
	}
}

func (p *Plugin) acquireLimitCountSecrets() (limitCountSecretSnapshot, func(), error) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if p.retired || (!p.legacySet && !p.scopedSet) {
		return limitCountSecretSnapshot{}, nil, errLimitCountCredentialsUnavailable
	}
	if p.activeUses == 0 {
		p.usesDone = make(chan struct{})
	}
	p.activeUses++
	return limitCountSecretSnapshot{
		legacy:    p.legacySet,
		keySecret: p.keySecret, redisHostSecret: p.redisHostSecret,
		redisClusterNodeSecrets: append([]*store.ResolvedSecret(nil), p.redisClusterNodeSecrets...),
		scopedKey:               p.scopedKeySecret, scopedRedisHost: p.scopedRedisHost,
		scopedRedisClusterNodes: append([]secret.Value(nil), p.scopedRedisClusterNodes...),
		keyPresent:              p.keyPresent, redisHostPresent: p.redisHostPresent,
		redisNodesPresent: p.redisNodesPresent,
		legacyKey:         p.legacyKey, legacyRedisHost: p.legacyRedisHost,
		legacyRedisNodes: append([]string(nil), p.legacyRedisNodes...),
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
		if p.limitCountAllowsUnpreparedLiteral() && !isLimitCountSecretReference(p.config.Key) {
			return use(p.config.Key)
		}
		return err
	}
	defer release()
	if !snapshot.keyPresent {
		return use(p.config.Key)
	}
	if !snapshot.legacy {
		return snapshot.scopedKey.Use(use)
	}
	if snapshot.keySecret == nil {
		return use(snapshot.legacyKey)
	}
	plaintext := snapshot.keySecret.Bytes()
	defer clear(plaintext)
	if len(plaintext) == 0 {
		return errLimitCountCredentialsUnavailable
	}
	return use(string(plaintext))
}

func (p *Plugin) withLimitCountRedisHost(use func(string) error) error {
	snapshot, release, err := p.acquireLimitCountSecrets()
	if err != nil {
		if p.limitCountAllowsUnpreparedLiteral() &&
			!isLimitCountSecretReference(p.config.Redis.RedisHost) {
			return use(p.config.Redis.RedisHost)
		}
		return err
	}
	defer release()
	if !snapshot.redisHostPresent {
		return use(p.config.Redis.RedisHost)
	}
	if !snapshot.legacy {
		return snapshot.scopedRedisHost.Use(use)
	}
	if snapshot.redisHostSecret == nil {
		return use(snapshot.legacyRedisHost)
	}
	plaintext := snapshot.redisHostSecret.Bytes()
	defer clear(plaintext)
	if len(plaintext) == 0 {
		return errLimitCountCredentialsUnavailable
	}
	return use(string(plaintext))
}

func (p *Plugin) withLimitCountRedisNodes(use func([]string) error) error {
	snapshot, release, err := p.acquireLimitCountSecrets()
	if err != nil {
		if p.limitCountAllowsUnpreparedLiteral() {
			if slices.ContainsFunc(
				p.config.RedisCluster.RedisClusterNodes,
				isLimitCountSecretReference,
			) {
				return err
			}
			return use(append([]string(nil), p.config.RedisCluster.RedisClusterNodes...))
		}
		return err
	}
	defer release()
	if !snapshot.redisNodesPresent {
		return use(append([]string(nil), p.config.RedisCluster.RedisClusterNodes...))
	}
	if snapshot.legacy {
		nodes := append([]string(nil), snapshot.legacyRedisNodes...)
		for index, owner := range snapshot.redisClusterNodeSecrets {
			if owner == nil {
				continue
			}
			plaintext := owner.Bytes()
			if len(plaintext) == 0 {
				for nodeIndex := range nodes {
					nodes[nodeIndex] = ""
				}
				return fmt.Errorf("limit-count Redis cluster node %d credential unavailable", index)
			}
			nodes[index] = string(plaintext)
			clear(plaintext)
		}
		err := use(nodes)
		for index := range nodes {
			nodes[index] = ""
		}
		return err
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

func (p *Plugin) limitCountCredentialDigests() (
	[sha256.Size]byte, [sha256.Size]byte, [][sha256.Size]byte,
) {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	if !p.retired && !p.legacySet && !p.scopedSet {
		_, hostSelection, nodesSelection := selectLimitCountSecretFields(p.config)
		hostDigest := sha256.Sum256([]byte(hostSelection.raw))
		nodes := selectedLimitCountNodes(p.config, nodesSelection)
		nodeDigests := make([][sha256.Size]byte, len(nodes))
		for index, node := range nodes {
			nodeDigests[index] = sha256.Sum256([]byte(node))
		}
		return [sha256.Size]byte{}, hostDigest, nodeDigests
	}
	return p.keyDigest, p.redisHostDigest, append([][sha256.Size]byte(nil), p.redisNodeDigests...)
}

func (p *Plugin) limitCountAllowsUnpreparedLiteral() bool {
	p.credentialMu.Lock()
	defer p.credentialMu.Unlock()
	return !p.retired && !p.legacySet && !p.scopedSet
}

func isLimitCountSecretReference(raw string) bool {
	upper := strings.ToUpper(raw)
	return strings.HasPrefix(upper, "$ENV://") || strings.HasPrefix(raw, "$secret://")
}
