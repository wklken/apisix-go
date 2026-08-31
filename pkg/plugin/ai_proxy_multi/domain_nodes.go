package ai_proxy_multi

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wklken/apisix-go/pkg/httpclient"
	"github.com/wklken/apisix-go/pkg/plugin/ai_common"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
)

type lookupNetIPFunc func(context.Context, string, string) ([]netip.Addr, error)

type nodeResolutionJob struct {
	index    int
	instance Instance
	endpoint *url.URL
}

type nodeResolutionResult struct {
	job       nodeResolutionJob
	addresses []netip.Addr
	err       error
}

type resolvedNode struct {
	ip           netip.Addr
	logicalHost  string
	client       *http.Client
	healthClient *http.Client
	health       instanceHealthState
	retired      atomic.Bool
}

func (n *resolvedNode) closeIdleConnections() {
	n.client.CloseIdleConnections()
	n.healthClient.CloseIdleConnections()
}

func (n *resolvedNode) finalizeIfRetired() {
	if n.retired.Load() {
		n.closeIdleConnections()
	}
}

type resolvedNodeResponseBody struct {
	body io.ReadCloser
	node *resolvedNode
	once sync.Once
	err  error
}

func (b *resolvedNodeResponseBody) Read(buffer []byte) (int, error) {
	return b.body.Read(buffer)
}

func (b *resolvedNodeResponseBody) Close() error {
	b.once.Do(func() {
		b.err = b.body.Close()
		b.node.finalizeIfRetired()
	})
	return b.err
}

type resolvedNodeSnapshot struct {
	instances  [][]*resolvedNode
	required   []bool
	resolveErr []error
}

type requestExecutionTarget struct {
	index int
	node  *resolvedNode
}

func (p *Plugin) initResolverDefaults() {
	if p.resolverTimeout <= 0 {
		p.resolverTimeout = 5 * time.Second
		if effective := p.StaticConfig(); effective != nil && effective.Config.Apisix.ResolverTimeout > 0 {
			p.resolverTimeout = time.Duration(effective.Config.Apisix.ResolverTimeout) * time.Second
		}
	}
	if p.resolverTTL <= 0 {
		p.resolverTTL = 30 * time.Second
		if effective := p.StaticConfig(); effective != nil && effective.Config.Apisix.DnsResolverValid > 0 {
			p.resolverTTL = time.Duration(effective.Config.Apisix.DnsResolverValid) * time.Second
		}
	}
	if p.lookupNetIP == nil {
		p.lookupNetIP = p.configuredNodeResolver().LookupNetIP
	}
	if p.nodeSets == nil {
		p.nodeSets = make(map[int][]*resolvedNode)
	}
	if p.nodeExpires == nil {
		p.nodeExpires = make(map[int]time.Time)
	}
	if p.nodeRequired == nil {
		p.nodeRequired = make(map[int]bool)
	}
	if p.nodeResolveErr == nil {
		p.nodeResolveErr = make(map[int]error)
	}
	if p.nodeRandom == nil {
		p.nodeRandom = rand.Intn
	}
}

func (p *Plugin) configuredNodeResolver() *net.Resolver {
	effective := p.StaticConfig()
	if effective == nil || len(effective.Config.Apisix.DnsResolver) == 0 {
		return net.DefaultResolver
	}
	servers := append([]string(nil), effective.Config.Apisix.DnsResolver...)
	var next atomic.Uint64
	return &net.Resolver{
		PreferGo:     true,
		StrictErrors: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			index := next.Add(1) - 1
			address := nodeDNSServerAddress(servers[index%uint64(len(servers))])
			return (&net.Dialer{Timeout: p.resolverTimeout}).DialContext(ctx, network, address)
		},
	}
}

func nodeDNSServerAddress(server string) string {
	server = strings.TrimSpace(server)
	if ip := net.ParseIP(server); ip != nil {
		return net.JoinHostPort(server, "53")
	}
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server
	}
	return net.JoinHostPort(server, "53")
}

func (p *Plugin) refreshResolvedNodes(ctx context.Context, force bool) error {
	p.nodeRefreshMu.Lock()
	defer p.nodeRefreshMu.Unlock()

	p.nodeMu.Lock()
	p.initResolverDefaults()
	now := p.healthNow
	current := time.Now()
	if now != nil {
		current = now()
	}
	var refreshErrors []error
	metadataChanged := false
	jobs := make([]nodeResolutionJob, 0, len(p.config.Instances))
	for index, instance := range p.config.Instances {
		parsed, known, err := instanceNodeEndpoint(instance)
		if !known {
			continue
		}
		if err != nil {
			metadataChanged = p.setNodeResolveErrorLocked(index, err) || metadataChanged
			refreshErrors = append(refreshErrors, err)
			continue
		}
		if parsed.Hostname() == "" || net.ParseIP(parsed.Hostname()) != nil {
			continue
		}
		if !p.nodeRequired[index] {
			p.nodeRequired[index] = true
			metadataChanged = true
		}
		if !force && current.Before(p.nodeExpires[index]) {
			continue
		}
		jobs = append(jobs, nodeResolutionJob{index: index, instance: instance, endpoint: parsed})
	}
	p.nodeMu.Unlock()

	results, resolutionComplete := p.resolveNodeJobs(ctx, jobs)
	sort.Slice(results, func(i, j int) bool { return results[i].job.index < results[j].job.index })
	if !resolutionComplete {
		refreshErrors = append(refreshErrors, errors.New("domain resolution worker pass incomplete"))
	}

	p.nodeMu.Lock()
	var retired []*resolvedNode
	snapshotChanged := false
	for _, result := range results {
		index := result.job.index
		instance := result.job.instance
		parsed := result.job.endpoint
		addresses := result.addresses
		err := result.err
		p.nodeExpires[index] = current.Add(p.resolverTTL)
		if err != nil || len(addresses) == 0 {
			if err == nil {
				err = errors.New("no addresses")
			}
			metadataChanged = p.setNodeResolveErrorLocked(index, err) || metadataChanged
			if len(p.nodeSets[index]) == 0 {
				refreshErrors = append(
					refreshErrors,
					fmt.Errorf("resolve instance %q endpoint: %w", instance.Name, err),
				)
			}
			continue
		}
		metadataChanged = p.setNodeResolveErrorLocked(index, nil) || metadataChanged
		addresses = sortedUniqueAddresses(addresses)
		oldByIP := make(map[netip.Addr]*resolvedNode, len(p.nodeSets[index]))
		for _, node := range p.nodeSets[index] {
			oldByIP[node.ip] = node
		}
		nodes := make([]*resolvedNode, 0, len(addresses))
		for _, address := range addresses {
			if node := oldByIP[address]; node != nil {
				nodes = append(nodes, node)
				delete(oldByIP, address)
				continue
			}
			nodes = append(nodes, p.newResolvedNode(index, parsed, address))
		}
		for _, node := range oldByIP {
			node.retired.Store(true)
			retired = append(retired, node)
		}
		if !slices.Equal(p.nodeSets[index], nodes) {
			snapshotChanged = true
		}
		p.nodeSets[index] = nodes
	}
	if snapshotChanged || metadataChanged {
		p.publishResolvedNodeSnapshotLocked()
		p.publishHealthSnapshot()
	}
	p.nodeMu.Unlock()
	for _, node := range retired {
		node.closeIdleConnections()
	}
	return errors.Join(refreshErrors...)
}

func (p *Plugin) resolveNodeJobs(ctx context.Context, jobs []nodeResolutionJob) ([]nodeResolutionResult, bool) {
	results := make([]nodeResolutionResult, 0, len(jobs))
	for _, job := range jobs {
		if ctx.Err() != nil {
			return results, false
		}
		lookupCtx, cancel := context.WithTimeout(ctx, p.resolverTimeout)
		addresses, err := p.lookupNetIP(lookupCtx, "ip", job.endpoint.Hostname())
		cancel()
		results = append(results, nodeResolutionResult{job: job, addresses: addresses, err: err})
		if ctx.Err() != nil {
			return results, false
		}
	}
	return results, true
}

func (p *Plugin) initializeResolvedNodeMetadata() {
	p.nodeMu.Lock()
	defer p.nodeMu.Unlock()
	p.initResolverDefaults()
	for index, instance := range p.config.Instances {
		parsed, known, err := instanceNodeEndpoint(instance)
		if !known {
			continue
		}
		if err != nil {
			p.setNodeResolveErrorLocked(index, err)
			continue
		}
		if parsed.Hostname() != "" && net.ParseIP(parsed.Hostname()) == nil {
			p.nodeRequired[index] = true
		}
	}
	p.publishResolvedNodeSnapshotLocked()
}

func (p *Plugin) setNodeResolveErrorLocked(index int, err error) bool {
	previous, exists := p.nodeResolveErr[index]
	if err == nil {
		if exists {
			delete(p.nodeResolveErr, index)
		}
		return exists
	}
	p.nodeResolveErr[index] = err
	return !exists || previous.Error() != err.Error()
}

func instanceNodeEndpoint(instance Instance) (*url.URL, bool, error) {
	endpoint, err := instanceHealthBaseURL(instance)
	if err != nil {
		return nil, false, nil
	}
	return endpoint, true, nil
}

func sortedUniqueAddresses(addresses []netip.Addr) []netip.Addr {
	addresses = slices.Clone(addresses)
	slices.SortFunc(addresses, func(left, right netip.Addr) int { return left.Compare(right) })
	return slices.Compact(addresses)
}

func (p *Plugin) newResolvedNode(index int, endpoint *url.URL, address netip.Addr) *resolvedNode {
	node := &resolvedNode{ip: address, logicalHost: endpoint.Hostname()}
	node.health.healthy = true
	node.client = &http.Client{Transport: p.resolvedNodeTransport(address, node.logicalHost, p.config.SSLVerify)}
	check := ActiveHealthCheck{}
	if p.config.Instances[index].Checks != nil {
		check = p.config.Instances[index].Checks.Active
	}
	verify := check.HTTPSVerifyCertificate
	node.healthClient = &http.Client{
		Transport: p.resolvedNodeTransport(address, node.logicalHost, verify),
		Timeout:   time.Duration(check.Timeout * float64(time.Second)),
	}
	if node.healthClient.Timeout <= 0 {
		node.healthClient.Timeout = time.Second
	}
	return node
}

func (p *Plugin) resolvedNodeTransport(address netip.Addr, logicalHost string, verify *bool) http.RoundTripper {
	transport := httpclient.NewTransport()
	ai_common.ApplyTransportKeepalive(transport, p.config.KeepalivePool, p.config.KeepaliveTimeout, p.config.Keepalive)
	ai_common.ApplyTransportSSLVerify(transport, verify)
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	transport.TLSClientConfig.ServerName = logicalHost
	timeout := time.Duration(p.config.Timeout) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	transport.DialContext = func(ctx context.Context, network, target string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(target)
		if err != nil {
			return nil, err
		}
		return (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext(
			ctx, network, net.JoinHostPort(address.String(), port),
		)
	}
	transport.TLSHandshakeTimeout = timeout
	transport.ResponseHeaderTimeout = timeout
	return pxy.NewProgressTimeoutTransport(transport, timeout, timeout)
}

func (p *Plugin) publishResolvedNodeSnapshotLocked() {
	instances := make([][]*resolvedNode, len(p.config.Instances))
	required := make([]bool, len(p.config.Instances))
	resolveErr := make([]error, len(p.config.Instances))
	for index := range instances {
		instances[index] = slices.Clone(p.nodeSets[index])
		required[index] = p.nodeRequired[index]
		resolveErr[index] = p.nodeResolveErr[index]
	}
	p.nodeSnapshot.Store(&resolvedNodeSnapshot{instances: instances, required: required, resolveErr: resolveErr})
}

func (p *Plugin) resolvedNodes(index int) []*resolvedNode {
	snapshot := p.nodeSnapshot.Load()
	if snapshot == nil || index < 0 || index >= len(snapshot.instances) {
		return nil
	}
	return snapshot.instances[index]
}

func (p *Plugin) hasDomainEndpoints() bool {
	snapshot := p.nodeSnapshot.Load()
	if snapshot == nil {
		return false
	}
	for _, required := range snapshot.required {
		if required {
			return true
		}
	}
	return false
}

func (p *Plugin) currentResolvedNode(index int, target *resolvedNode) bool {
	return slices.Contains(p.resolvedNodes(index), target)
}

func (p *Plugin) pickResolvedNode(index int) *resolvedNode {
	return p.pickResolvedNodeFromSnapshot(index, p.snapshot.Load())
}

func (p *Plugin) pickResolvedNodeFromSnapshot(index int, snapshot *healthSnapshot) *resolvedNode {
	if snapshot == nil || index < 0 || index >= len(snapshot.allNodes) {
		return nil
	}
	nodes := snapshot.healthyNodes[index]
	if len(nodes) == 0 {
		nodes = snapshot.allNodes[index]
	}
	if len(nodes) == 0 {
		return nil
	}
	p.mu.Lock()
	slot := p.nodeRandom(len(nodes))
	p.mu.Unlock()
	return nodes[slot]
}

func (p *Plugin) pickResolvedNodeForRequestFromSnapshot(
	index int, snapshot *healthSnapshot,
) (*resolvedNode, error) {
	node := p.pickResolvedNodeFromSnapshot(index, snapshot)
	if node != nil || snapshot == nil || index < 0 || index >= len(snapshot.nodeRequired) ||
		!snapshot.nodeRequired[index] {
		return node, nil
	}
	if err := snapshot.nodeResolveErr[index]; err != nil {
		return nil, fmt.Errorf("resolve instance %q endpoint: %w", p.config.Instances[index].Name, err)
	}
	return nil, fmt.Errorf("resolve instance %q endpoint: no addresses", p.config.Instances[index].Name)
}

func (p *Plugin) pickExecutionTarget(
	r *http.Request, tried map[int]bool,
) (requestExecutionTarget, bool, error) {
	snapshot := p.snapshot.Load()
	index, ok := p.pickInstanceFromSnapshot(r, tried, snapshot)
	if !ok {
		return requestExecutionTarget{}, false, nil
	}
	node, err := p.pickResolvedNodeForRequestFromSnapshot(index, snapshot)
	if err != nil {
		return requestExecutionTarget{}, false, err
	}
	return requestExecutionTarget{index: index, node: node}, true, nil
}
