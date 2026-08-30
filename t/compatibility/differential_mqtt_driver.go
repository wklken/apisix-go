package pluginintegration

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"
)

const differentialMQTTOracleListenPort = 1985

func isDifferentialMQTTProxyCase(spec DifferentialCase) bool {
	return spec.ComparisonPolicy == differentialMQTTProxyCONNECTPolicy
}

func projectDifferentialMQTTListenPort(config map[string]any, port int) error {
	if port <= 0 {
		return fmt.Errorf("MQTT stream listen port = %d", port)
	}
	replaced := replaceDifferentialMQTTListenPort(config, port)
	if replaced != 1 {
		return fmt.Errorf("MQTT stream listen placeholder replacements = %d, want 1", replaced)
	}
	return nil
}

func replaceDifferentialMQTTListenPort(value any, port int) int {
	switch typed := value.(type) {
	case map[string]any:
		replaced := 0
		for key, child := range typed {
			if child == differentialMQTTListenPortPlaceholder {
				typed[key] = port
				replaced++
				continue
			}
			replaced += replaceDifferentialMQTTListenPort(child, port)
		}
		return replaced
	case []any:
		replaced := 0
		for index, child := range typed {
			if child == differentialMQTTListenPortPlaceholder {
				typed[index] = port
				replaced++
				continue
			}
			replaced += replaceDifferentialMQTTListenPort(child, port)
		}
		return replaced
	default:
		return 0
	}
}

func renderDifferentialMQTTRuntime(base []byte, listenAddress string) ([]byte, error) {
	if strings.TrimSpace(listenAddress) == "" {
		return nil, errors.New("MQTT stream listen address is empty")
	}
	var runtime map[string]any
	if err := yaml.Unmarshal(base, &runtime); err != nil {
		return nil, fmt.Errorf("decode base differential runtime: %w", err)
	}
	overlay := differentialMQTTProxyRuntimeOverlay(listenAddress)
	if err := validateDifferentialMQTTRuntimeOverlay(overlay, listenAddress); err != nil {
		return nil, err
	}
	mergeDifferentialRuntimeMap(runtime, overlay)
	return yaml.Marshal(runtime)
}

func validateDifferentialMQTTRuntimeOverlay(overlay map[string]any, listenAddress string) error {
	if len(overlay) != 2 {
		return fmt.Errorf("MQTT runtime overlay keys = %d, want 2", len(overlay))
	}
	apisix, ok := overlay["apisix"].(map[string]any)
	if !ok || len(apisix) != 2 || apisix["proxy_mode"] != "http&stream" {
		return errors.New("MQTT runtime overlay must own only proxy_mode and stream_proxy")
	}
	streamProxy, ok := apisix["stream_proxy"].(map[string]any)
	if !ok || len(streamProxy) != 1 {
		return errors.New("MQTT runtime overlay stream_proxy is not exact")
	}
	tcp, ok := streamProxy["tcp"].([]any)
	if !ok || len(tcp) != 1 {
		return errors.New("MQTT runtime overlay must declare one TCP listener")
	}
	listener, ok := tcp[0].(map[string]any)
	if !ok || len(listener) != 1 || listener["addr"] != listenAddress {
		return errors.New("MQTT runtime overlay TCP listener is not exact")
	}
	plugins, ok := overlay["stream_plugins"].([]any)
	if !ok || len(plugins) != 1 || plugins[0] != "mqtt-proxy" {
		return errors.New("MQTT runtime overlay stream_plugins is not exact")
	}
	return nil
}

func startDifferentialMQTTCandidateUnderStartupLock(
	workDir string,
	logPath string,
	binary string,
	runtimePath string,
	standalonePath string,
	config map[string]any,
	fixtureEndpoint string,
	plugins []string,
) (*differentialChild, int, int, int, int, map[string]any, error) {
	integrationStartupMu.Lock()
	defer integrationStartupMu.Unlock()

	candidatePort, err := reservePort()
	if err != nil {
		return nil, 0, 0, 0, 0, nil, fmt.Errorf("reserve candidate port: %w", err)
	}
	statusPort, err := reserveDifferentialPortExcluding(candidatePort)
	if err != nil {
		return nil, 0, 0, 0, 0, nil, fmt.Errorf("reserve candidate status port: %w", err)
	}
	controlPort, err := reserveDifferentialPortExcluding(candidatePort, statusPort)
	if err != nil {
		return nil, 0, 0, 0, 0, nil, fmt.Errorf("reserve candidate control port: %w", err)
	}
	streamPort, err := reserveDifferentialPortExcluding(candidatePort, statusPort, controlPort)
	if err != nil {
		return nil, 0, 0, 0, 0, nil, fmt.Errorf("reserve candidate MQTT port: %w", err)
	}

	projected, err := projectDifferentialConfig(config, fixtureEndpoint)
	if err != nil {
		return nil, 0, 0, 0, 0, nil, fmt.Errorf("project candidate MQTT config: %w", err)
	}
	if err := projectDifferentialMQTTListenPort(projected, streamPort); err != nil {
		return nil, 0, 0, 0, 0, nil, err
	}
	standalone, err := renderDifferentialStandalone(projected, "")
	if err != nil {
		return nil, 0, 0, 0, 0, nil, fmt.Errorf("render candidate MQTT standalone: %w", err)
	}
	if err := os.WriteFile(standalonePath, standalone, 0o600); err != nil {
		return nil, 0, 0, 0, 0, nil, fmt.Errorf("write candidate MQTT standalone: %w", err)
	}
	baseRuntime, err := renderDifferentialCandidateRuntimeWithOverlay(
		candidatePort, statusPort, controlPort, workDir, plugins, nil,
	)
	if err != nil {
		return nil, 0, 0, 0, 0, nil, fmt.Errorf("render candidate MQTT base runtime: %w", err)
	}
	runtime, err := renderDifferentialMQTTRuntime(
		baseRuntime, net.JoinHostPort("127.0.0.1", strconv.Itoa(streamPort)),
	)
	if err != nil {
		return nil, 0, 0, 0, 0, nil, fmt.Errorf("render candidate MQTT runtime: %w", err)
	}
	if err := os.WriteFile(runtimePath, runtime, 0o600); err != nil {
		return nil, 0, 0, 0, 0, nil, fmt.Errorf("write candidate MQTT runtime: %w", err)
	}
	child, err := startDifferentialCandidate(workDir, logPath, binary, runtimePath)
	if err != nil {
		return nil, 0, 0, 0, 0, nil, err
	}
	if err := waitDifferentialCandidateListeners(child, candidatePort, statusPort, controlPort); err != nil {
		return child, candidatePort, statusPort, controlPort, streamPort, projected, err
	}
	if err := waitDifferentialListener(child, streamPort, 5*time.Second); err != nil {
		return child, candidatePort, statusPort, controlPort, streamPort, projected, err
	}
	return child, candidatePort, statusPort, controlPort, streamPort, projected, nil
}

func differentialMQTTListenEndpoint(spec DifferentialCase) (string, error) {
	routes, ok := spec.Config["stream_routes"].([]any)
	if !ok || len(routes) != 1 {
		return "", errors.New("MQTT differential config must contain one stream route")
	}
	route, ok := routes[0].(map[string]any)
	if !ok {
		return "", errors.New("MQTT differential stream route is not an object")
	}
	host, ok := route["server_addr"].(string)
	if !ok || strings.TrimSpace(host) == "" {
		return "", errors.New("MQTT differential server_addr is empty")
	}
	port, ok := route["server_port"].(int)
	if !ok || port <= 0 {
		return "", fmt.Errorf("MQTT differential server_port = %#v", route["server_port"])
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func observeDifferentialMQTTProxyCandidate(
	fixture *differentialFixtureServer,
	spec DifferentialCase,
	upstreamAddress string,
) (DifferentialObservation, error) {
	endpoint, err := differentialMQTTListenEndpoint(spec)
	if err != nil {
		return DifferentialObservation{}, err
	}
	return observeDifferentialMQTTProxyExchanges(
		fixture,
		spec,
		upstreamAddress,
		func(payload []byte, allowNoResponse bool) ([]byte, error) {
			return executeDifferentialMQTTExchange(endpoint, payload, allowNoResponse)
		},
	)
}

func observeDifferentialMQTTProxyOracle(
	fixture *differentialFixtureServer,
	spec DifferentialCase,
	child *differentialChild,
	upstreamAddress string,
) (DifferentialObservation, error) {
	endpoint, err := differentialMQTTListenEndpoint(spec)
	if err != nil {
		return DifferentialObservation{}, err
	}
	_, portText, err := net.SplitHostPort(endpoint)
	if err != nil {
		return DifferentialObservation{}, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return DifferentialObservation{}, err
	}
	return observeDifferentialMQTTProxyExchanges(
		fixture,
		spec,
		upstreamAddress,
		func(payload []byte, allowNoResponse bool) ([]byte, error) {
			return executeDifferentialMQTTOracleExchange(child, port, payload, allowNoResponse)
		},
	)
}

func observeDifferentialMQTTProxyExchanges(
	fixture *differentialFixtureServer,
	spec DifferentialCase,
	upstreamAddress string,
	exchange func([]byte, bool) ([]byte, error),
) (DifferentialObservation, error) {
	if fixture == nil || exchange == nil || !isDifferentialMQTTProxyCase(spec) {
		return DifferentialObservation{}, errors.New("MQTT differential driver requires its pinned case and fixture")
	}
	observation := DifferentialObservation{Steps: make([]DifferentialStepObservation, 0, len(spec.Steps))}
	for index, step := range spec.Steps {
		response, err := exchange([]byte(step.Request.Body), index == 0)
		if err != nil {
			return DifferentialObservation{}, fmt.Errorf("MQTT exchange step %d: %w", index, err)
		}
		observation.Steps = append(observation.Steps, DifferentialStepObservation{
			Body: string(response), SecurityDecision: step.SecurityDecision,
		})
	}
	captured, err := fixture.collectWithTimeout(
		spec.Fixture.ExpectedCalls,
		differentialCandidateFixtureCollectTimeout(spec.Fixture),
	)
	if err != nil {
		return DifferentialObservation{}, err
	}
	applyDifferentialSequenceFixtureObservation(
		&observation, spec.Fixture, captured, upstreamAddress,
	)
	return observation, nil
}

func executeDifferentialMQTTExchange(
	endpoint string,
	payload []byte,
	allowNoResponse bool,
) ([]byte, error) {
	connection, err := net.DialTimeout("tcp", endpoint, time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial MQTT gateway %s: %w", endpoint, err)
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := connection.Write(payload); err != nil {
		return nil, fmt.Errorf("write MQTT gateway payload: %w", err)
	}
	response, err := io.ReadAll(io.LimitReader(connection, differentialMQTTMaxPacketBytes+1))
	if err != nil && (!allowNoResponse || !isDifferentialMQTTNoResponse(err)) {
		return nil, fmt.Errorf("read MQTT gateway response: %w", err)
	}
	if len(response) > differentialMQTTMaxPacketBytes {
		return nil, fmt.Errorf("MQTT gateway response exceeds %d bytes", differentialMQTTMaxPacketBytes)
	}
	return response, nil
}

func isDifferentialMQTTNoResponse(err error) bool {
	if errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

func executeDifferentialMQTTOracleExchange(
	child *differentialChild,
	port int,
	payload []byte,
	allowNoResponse bool,
) ([]byte, error) {
	if child == nil || !child.container || port <= 0 {
		return nil, errors.New("MQTT Oracle exchange requires a running container and port")
	}
	allowNoResponseValue := "0"
	if allowNoResponse {
		allowNoResponseValue = "1"
	}
	output, err := runDifferentialPodmanCommand(
		child.runtime,
		differentialPodmanTimeout,
		nil,
		nil,
		"exec",
		child.name,
		"perl",
		"-MIO::Socket::INET",
		"-MIO::Select",
		"-MErrno=ECONNRESET",
		"-e",
		`my ($port, $hex, $allow_no_response) = @ARGV; my $socket; for (1..30) { $socket = IO::Socket::INET->new(PeerAddr => "127.0.0.1", PeerPort => $port, Proto => "tcp", Timeout => 1); last if $socket; select(undef, undef, undef, 0.1); } die "connect\n" unless $socket; binmode STDOUT; my $payload = pack("H*", $hex); my $offset = 0; while ($offset < length($payload)) { my $count = syswrite($socket, $payload, length($payload) - $offset, $offset); die "write: $!\n" unless defined $count; $offset += $count; } my $select = IO::Select->new($socket); while (1) { if ($allow_no_response) { my @ready = $select->can_read(1); last unless @ready; } my $count = sysread($socket, my $buffer, 8192); if (!defined $count) { last if 0 + $! == ECONNRESET; die "read: $!\n"; } last if $count == 0; print STDOUT $buffer; } close($socket);`,
		strconv.Itoa(port),
		hex.EncodeToString(payload),
		allowNoResponseValue,
	)
	if err != nil {
		return nil, fmt.Errorf("execute MQTT Oracle exchange: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return output, nil
}
