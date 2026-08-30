package pluginintegration

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDifferentialTargetPortUsesDataByDefaultAndControlExplicitly(t *testing.T) {
	tests := []struct {
		name    string
		target  DifferentialRequestTarget
		want    int
		wantErr bool
	}{
		{name: "zero value data plane", want: 19080},
		{name: "explicit data plane", target: DifferentialRequestTargetData, want: 19080},
		{name: "control", target: DifferentialRequestTargetControl, want: 19090},
		{name: "unsupported", target: DifferentialRequestTarget("admin"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := differentialTargetPort(test.target, 19080, 19090)
			if test.wantErr {
				if err == nil {
					t.Fatalf("differentialTargetPort() = %d, want error", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("differentialTargetPort() = %d/%v, want %d", got, err, test.want)
			}
		})
	}

	if differentialRequestIsZero(DifferentialRequest{Target: DifferentialRequestTargetControl}) {
		t.Fatal("control-target request is treated as a zero request")
	}
}

func TestObserveDifferentialSideTargetsCandidateControlListener(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{Name: "unused"})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.close()

	var dataCalls atomic.Int32
	data := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dataCalls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer data.Close()
	var controlCalls atomic.Int32
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		controlCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer control.Close()

	dataPort := data.Listener.Addr().(*net.TCPAddr).Port
	controlPort := control.Listener.Addr().(*net.TCPAddr).Port
	spec := DifferentialCase{
		Name: "control-target",
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/v1/server_info",
			Target: DifferentialRequestTargetControl,
		},
		Fixture: DifferentialFixture{Name: "unused"},
	}
	observation, err := observeDifferentialSideWithPorts(
		fixture, spec, dataPort, controlPort, "127.0.0.1:1980",
	)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Status != http.StatusOK || controlCalls.Load() != 1 || dataCalls.Load() != 0 {
		t.Fatalf(
			"observation/data/control = %d/%d/%d, want 200/0/1",
			observation.Status, dataCalls.Load(), controlCalls.Load(),
		)
	}
}

func TestObserveDifferentialSequenceTargetsEachCandidateListener(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{Name: "unused"})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.close()

	data := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer data.Close()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer control.Close()

	spec := DifferentialCase{
		Name: "mixed-target-sequence",
		Steps: []DifferentialStep{
			{Request: DifferentialRequest{Method: http.MethodGet, Path: "/data"}},
			{Request: DifferentialRequest{
				Method: http.MethodGet, Path: "/control", Target: DifferentialRequestTargetControl,
			}},
		},
		Fixture: DifferentialFixture{Name: "unused"},
	}
	observation, err := observeDifferentialSideWithPorts(
		fixture,
		spec,
		data.Listener.Addr().(*net.TCPAddr).Port,
		control.Listener.Addr().(*net.TCPAddr).Port,
		"127.0.0.1:1980",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Steps) != 2 || observation.Steps[0].Status != http.StatusNoContent ||
		observation.Steps[1].Status != http.StatusOK {
		t.Fatalf("sequence observations = %#v, want data 204 then control 200", observation.Steps)
	}
}

func TestObserveDifferentialSideRunsSequenceAgainstOneProcessAndCollectsFixtureOnce(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{
		Name:          "primary",
		Response:      DifferentialFixtureResponse{Status: http.StatusOK, Body: "upstream"},
		ExpectedCalls: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.close()

	var requests atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			response, requestErr := http.Get(fixture.server.URL + r.URL.RequestURI())
			if requestErr != nil {
				http.Error(w, requestErr.Error(), http.StatusBadGateway)
				return
			}
			defer func() { _ = response.Body.Close() }()
			w.WriteHeader(response.StatusCode)
			_, _ = io.Copy(w, response.Body)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer gateway.Close()

	spec := DifferentialCase{
		Name: "stateful-sequence",
		Steps: []DifferentialStep{
			{
				Request: DifferentialRequest{
					Method: http.MethodGet,
					Path:   "/hello",
					Host:   "gateway.example.test",
				},
				SecurityDecision: "allow",
			},
			{
				Request: DifferentialRequest{
					Method: http.MethodGet,
					Path:   "/hello",
					Host:   "gateway.example.test",
				},
				SecurityDecision: "deny",
			},
		},
		Fixture: DifferentialFixture{Name: "primary", ExpectedCalls: 1},
	}
	port := gateway.Listener.Addr().(*net.TCPAddr).Port
	observation, err := observeDifferentialSide(
		fixture, spec, port, net.JoinHostPort("127.0.0.1", fmt.Sprint(fixture.port())),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Steps) != 2 || observation.Steps[0].Status != http.StatusOK ||
		observation.Steps[1].Status != http.StatusServiceUnavailable {
		t.Fatalf("sequence observations = %#v", observation.Steps)
	}
	if observation.Steps[0].SecurityDecision != "allow" || observation.Steps[1].SecurityDecision != "deny" {
		t.Fatalf("sequence security decisions = %#v", observation.Steps)
	}
	if !observation.Upstream.Received || observation.Upstream.Path != "/hello" || observation.RetryCount != 0 {
		t.Fatalf("fixture observation = %#v, retries=%d", observation.Upstream, observation.RetryCount)
	}
}

func TestObserveDifferentialSideHonorsStepDelayBeforeRequest(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{
		Name: "primary", Response: DifferentialFixtureResponse{Status: http.StatusOK},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.close()

	requests := make(chan time.Time, 2)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests <- time.Now()
		w.WriteHeader(http.StatusOK)
	}))
	defer gateway.Close()

	spec := DifferentialCase{
		Name: "delayed-sequence",
		Steps: []DifferentialStep{
			{Request: DifferentialRequest{Method: http.MethodGet, Path: "/first"}},
			{DelayBeforeMillis: 100, Request: DifferentialRequest{Method: http.MethodGet, Path: "/second"}},
		},
		Fixture: DifferentialFixture{Name: "primary"},
	}
	port := gateway.Listener.Addr().(*net.TCPAddr).Port
	if _, err := observeDifferentialSide(fixture, spec, port, "127.0.0.1:1980"); err != nil {
		t.Fatal(err)
	}
	first := <-requests
	second := <-requests
	if delay := second.Sub(first); delay < 80*time.Millisecond {
		t.Fatalf("delay between sequence requests = %s, want at least 80ms", delay)
	}
}

func TestObserveDifferentialSideRequestWindowExcludesStartupCalls(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{
		Name: "request-window", Response: DifferentialFixtureResponse{Status: http.StatusNoContent},
		ExpectedCalls: 1, CollectTimeoutMillis: 500, RequestWindowQuietMillis: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.close()

	if _, err := http.Get(fixture.server.URL + "/startup"); err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response, requestErr := http.Get(fixture.server.URL + r.URL.RequestURI())
		if requestErr != nil {
			http.Error(w, requestErr.Error(), http.StatusBadGateway)
			return
		}
		_ = response.Body.Close()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer gateway.Close()

	spec := DifferentialCase{
		Name: "request-window",
		Steps: []DifferentialStep{
			{
				Request: DifferentialRequest{
					Method: http.MethodGet,
					Path:   "/request",
					Host:   "gateway.example.test",
				},
				SecurityDecision: "allow",
			},
		},
		Fixture: DifferentialFixture{
			Name: "request-window", ExpectedCalls: 1,
			CollectTimeoutMillis: 500, RequestWindowQuietMillis: 40,
		},
	}
	port := gateway.Listener.Addr().(*net.TCPAddr).Port
	observation, err := observeDifferentialSide(fixture, spec, port, "127.0.0.1:1980")
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.UpstreamCalls) != 1 || observation.UpstreamCalls[0].Path != "/request" {
		t.Fatalf("request-window calls = %#v, want only /request", observation.UpstreamCalls)
	}
}

func TestDifferentialFixtureRequestWindowRejectsDroppedCalls(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{
		Name: "request-window-drop", Response: DifferentialFixtureResponse{Status: http.StatusNoContent},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.close()

	baseline, err := fixture.beginRequestWindow(10*time.Millisecond, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < cap(fixture.requests)+1; index++ {
		response, requestErr := http.Get(fmt.Sprintf("%s/window-%d", fixture.server.URL, index))
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		_ = response.Body.Close()
	}
	_, err = fixture.collectRequestWindow(
		baseline, cap(fixture.requests), 200*time.Millisecond, 10*time.Millisecond,
	)
	if err == nil || !strings.Contains(err.Error(), "dropped") {
		t.Fatalf("collect dropped request-window calls error = %v", err)
	}
}

func TestDifferentialFixtureRequestWindowRejectsExtraCall(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{
		Name: "request-window-extra", Response: DifferentialFixtureResponse{Status: http.StatusNoContent},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.close()

	baseline, err := fixture.beginRequestWindow(10*time.Millisecond, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/expected", "/extra"} {
		response, requestErr := http.Get(fixture.server.URL + path)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		_ = response.Body.Close()
	}
	_, err = fixture.collectRequestWindow(baseline, 1, 200*time.Millisecond, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "want exactly 1") {
		t.Fatalf("collect extra request-window call error = %v", err)
	}
}

func TestDifferentialFixtureRequestWindowFailsWhenStartupNeverQuiesces(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{
		Name: "request-window-busy", Response: DifferentialFixtureResponse{Status: http.StatusNoContent},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.close()

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			response, requestErr := http.Get(fixture.server.URL + "/startup")
			if requestErr == nil {
				_ = response.Body.Close()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	_, err = fixture.beginRequestWindow(30*time.Millisecond, 80*time.Millisecond)
	close(stop)
	<-done
	if err == nil || !strings.Contains(err.Error(), "did not become quiet") {
		t.Fatalf("begin busy request window error = %v", err)
	}
}

func TestObserveDifferentialSideRunsConcurrentRequestsAsOneBatch(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{
		Name: "primary", Response: DifferentialFixtureResponse{Status: http.StatusOK},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.close()

	const concurrentRequests = 4
	var active atomic.Int32
	var maximum atomic.Int32
	var entered atomic.Int32
	release := make(chan struct{})
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/probe" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		if entered.Add(1) == concurrentRequests {
			close(release)
		}
		select {
		case <-release:
		case <-time.After(2 * time.Second):
			http.Error(w, "concurrent batch never entered", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer gateway.Close()

	batch := make([]DifferentialRequest, 0, concurrentRequests)
	for index := range concurrentRequests {
		batch = append(batch, DifferentialRequest{
			Method: http.MethodGet,
			Path:   fmt.Sprintf("/route-%d", index%2),
			Host:   "gateway.example.test",
		})
	}
	spec := DifferentialCase{
		Name: "concurrent-batch",
		Steps: []DifferentialStep{
			{ConcurrentRequests: batch, SecurityDecision: "mixed"},
			{
				Request: DifferentialRequest{
					Method: http.MethodGet, Path: "/probe", Host: "gateway.example.test",
				},
				SecurityDecision: "allow",
			},
		},
		Fixture: DifferentialFixture{Name: "primary"},
	}
	port := gateway.Listener.Addr().(*net.TCPAddr).Port
	observation, err := observeDifferentialSide(fixture, spec, port, "127.0.0.1:1980")
	if err != nil {
		t.Fatal(err)
	}
	if got := maximum.Load(); got != concurrentRequests {
		t.Fatalf("maximum concurrent requests = %d, want %d", got, concurrentRequests)
	}
	if len(observation.Steps) != concurrentRequests+1 {
		t.Fatalf("step observation count = %d, want %d", len(observation.Steps), concurrentRequests+1)
	}
	for index, step := range observation.Steps[:concurrentRequests] {
		if step.Status != http.StatusOK || step.SecurityDecision != "mixed" {
			t.Fatalf("concurrent observation %d = %#v", index, step)
		}
	}
	if probe := observation.Steps[concurrentRequests]; probe.Status != http.StatusNoContent ||
		probe.SecurityDecision != "allow" {
		t.Fatalf("probe observation = %#v", probe)
	}
}

func TestObserveDifferentialSideRejectsAmbiguousConcurrentStep(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{
		Name: "primary", Response: DifferentialFixtureResponse{Status: http.StatusOK},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.close()

	spec := DifferentialCase{
		Name: "ambiguous-concurrent-step",
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{Method: http.MethodGet, Path: "/single"},
			ConcurrentRequests: []DifferentialRequest{{
				Method: http.MethodGet, Path: "/concurrent",
			}},
		}},
		Fixture: DifferentialFixture{Name: "primary"},
	}
	_, err = observeDifferentialSide(fixture, spec, 1, "127.0.0.1:1980")
	if err == nil || !strings.Contains(err.Error(), "cannot set both request and concurrent_requests") {
		t.Fatalf("observe ambiguous concurrent step error = %v", err)
	}
}

func TestDifferentialHTTPFixtureDelaysResponseAfterCapture(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{
		Name: "delayed",
		Response: DifferentialFixtureResponse{
			Status:      http.StatusOK,
			Body:        "delayed response",
			DelayMillis: 100,
		},
		ExpectedCalls: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.close()

	started := time.Now()
	response, err := http.Get(fixture.server.URL + "/delay")
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close delayed response: %v / %v", readErr, closeErr)
	}
	if elapsed := time.Since(started); elapsed < 80*time.Millisecond {
		t.Fatalf("fixture response delay = %s, want at least 80ms", elapsed)
	}
	captured, err := fixture.collect(1)
	if err != nil || len(captured) != 1 || captured[0].Path != "/delay" {
		t.Fatalf("delayed fixture capture = %#v, %v", captured, err)
	}
}

func TestDifferentialFixtureRejectsInvalidResponseDelay(t *testing.T) {
	for _, delay := range []int{-1, 5001} {
		_, err := startDifferentialFixture(DifferentialFixture{
			Name:     "invalid-delay",
			Response: DifferentialFixtureResponse{DelayMillis: delay},
		})
		if err == nil || !strings.Contains(err.Error(), "delay_millis") {
			t.Fatalf("start fixture with delay %d error = %v", delay, err)
		}
	}
}

func TestDifferentialFixtureRequestWindowValidationFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name    string
		fixture DifferentialFixture
		want    string
	}{
		{
			name: "negative quiet interval",
			fixture: DifferentialFixture{
				ExpectedCalls: 1, RequestWindowQuietMillis: -1,
			},
			want: "request_window_quiet_millis",
		},
		{
			name: "window without expected call",
			fixture: DifferentialFixture{
				RequestWindowQuietMillis: 1,
			},
			want: "expected_calls",
		},
		{
			name: "quiet interval reaches collect timeout",
			fixture: DifferentialFixture{
				ExpectedCalls: 1, RequestWindowQuietMillis: 100, CollectTimeoutMillis: 100,
			},
			want: "collect_timeout_millis",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateDifferentialFixtureRequestWindow(test.fixture); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("validate request window error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestApplyDifferentialSequenceFixtureObservationPreservesCallsWithoutRetries(t *testing.T) {
	observation := DifferentialObservation{}
	received := []differentialCapturedRequest{
		{
			Method: http.MethodGet, Path: "/first", Host: "upstream.example.test",
			Headers: http.Header{"X-Route-Observer": {"first"}},
		},
		{
			Method: http.MethodPost, Path: "/second", Host: "upstream.example.test",
			Headers: http.Header{"Content-Type": {"application/json"}}, Body: `{"step":2}`,
		},
	}

	applyDifferentialSequenceFixtureObservation(
		&observation,
		DifferentialFixture{Name: "primary", ExpectedCalls: 2},
		received,
		"127.0.0.1:1980",
	)

	if observation.RetryCount != 0 {
		t.Fatalf("sequence retry count = %d, want 0", observation.RetryCount)
	}
	if len(observation.UpstreamCalls) != 2 {
		t.Fatalf("sequence upstream calls = %#v, want two", observation.UpstreamCalls)
	}
	if observation.UpstreamCalls[0].Path != "/first" ||
		observation.UpstreamCalls[1].Path != "/second" ||
		observation.UpstreamCalls[1].Body != `{"step":2}` {
		t.Fatalf("sequence upstream calls = %#v", observation.UpstreamCalls)
	}
	if observation.Upstream.Path != "/second" {
		t.Fatalf("legacy upstream projection = %#v, want final call", observation.Upstream)
	}
}

func TestCollectDifferentialOracleFixturesReturnsExpectedAndExtraCallsInOrder(t *testing.T) {
	root := t.TempDir()
	runtimePath := filepath.Join(root, "fake-container")
	script := `#!/bin/sh
for last_arg do :; done
index=${last_arg##*.}
record="$FAKE_ORACLE_RECORD_DIR/$index"
case "$*" in
  *sysread*)
    [ -f "$record" ] || exit 1
    /bin/cat "$record"
    ;;
  *)
    [ -f "$record" ]
    ;;
esac
`
	if err := os.WriteFile(runtimePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_ORACLE_RECORD_DIR", root)
	requests := []string{
		"GET /first HTTP/1.1\r\nHost: upstream.example.test\r\n\r\n",
		"POST /second HTTP/1.1\r\nHost: upstream.example.test\r\nContent-Length: 4\r\n\r\nbody",
		"GET /extra HTTP/1.1\r\nHost: upstream.example.test\r\n\r\n",
	}
	for index, request := range requests {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprint(index)), []byte(request), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	child := &differentialChild{runtime: runtimePath, name: "fake-oracle"}

	captured, err := collectDifferentialOracleFixtures(child, 2)
	if err != nil {
		t.Fatalf("collectDifferentialOracleFixtures() error = %v", err)
	}
	if len(captured) != 3 {
		t.Fatalf("captured request count = %d, want expected two plus one extra", len(captured))
	}
	if captured[0].Path != "/first" || captured[1].Path != "/second" ||
		captured[2].Path != "/extra" {
		t.Fatalf("captured request order = %#v", captured)
	}
}

func TestDifferentialOracleRequestWindowCollectsOnlyRecordsAfterBaseline(t *testing.T) {
	child, root := newDifferentialOracleRecordTestRuntime(t)
	writeDifferentialOracleRecordTestFile(t, root, 0, "GET /startup HTTP/1.1\r\nHost: startup.example.test\r\n\r\n")

	baseline, err := beginDifferentialOracleRequestWindow(
		child, 20*time.Millisecond, 2*time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	if baseline != 1 {
		t.Fatalf("Oracle request-window baseline = %d, want 1", baseline)
	}
	writeDifferentialOracleRecordTestFile(t, root, 1, "GET /request HTTP/1.1\r\nHost: request.example.test\r\n\r\n")
	captured, err := collectDifferentialOracleFixtureWindow(
		child, baseline, 1, 2*time.Second, 20*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(captured) != 1 || captured[0].Path != "/request" {
		t.Fatalf("Oracle request-window calls = %#v, want only /request", captured)
	}
}

func TestDifferentialOracleRequestWindowRejectsRecordDiscontinuity(t *testing.T) {
	child, root := newDifferentialOracleRecordTestRuntime(t)
	writeDifferentialOracleRecordTestFile(t, root, 0, "GET /startup HTTP/1.1\r\nHost: startup.example.test\r\n\r\n")
	writeDifferentialOracleRecordTestFile(t, root, 2, "GET /gap HTTP/1.1\r\nHost: gap.example.test\r\n\r\n")

	_, err := beginDifferentialOracleRequestWindow(child, 20*time.Millisecond, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "not contiguous") {
		t.Fatalf("begin discontinuous Oracle request window error = %v", err)
	}
}

func TestDifferentialOracleRequestWindowRechecksEarlierRecordBeforeReportingDiscontinuity(t *testing.T) {
	child, root := newDifferentialOracleRecordTestRuntime(t)
	writeDifferentialOracleRecordTestFile(t, root, 0, "GET /first HTTP/1.1\r\nHost: first.example.test\r\n\r\n")
	writeDifferentialOracleRecordTestFile(t, root, 1, "GET /second HTTP/1.1\r\nHost: second.example.test\r\n\r\n")
	t.Setenv("FAKE_ORACLE_HIDE_ZERO_ONCE", "1")

	baseline, err := beginDifferentialOracleRequestWindow(child, 20*time.Millisecond, 2*time.Second)
	if err != nil {
		t.Fatalf("begin raced Oracle request window error = %v", err)
	}
	if baseline != 2 {
		t.Fatalf("Oracle request-window baseline = %d, want 2", baseline)
	}
}

func TestDifferentialOracleRequestWindowRejectsExtraCall(t *testing.T) {
	child, root := newDifferentialOracleRecordTestRuntime(t)
	writeDifferentialOracleRecordTestFile(t, root, 0, "GET /request HTTP/1.1\r\nHost: request.example.test\r\n\r\n")
	writeDifferentialOracleRecordTestFile(t, root, 1, "GET /extra HTTP/1.1\r\nHost: extra.example.test\r\n\r\n")

	_, err := collectDifferentialOracleFixtureWindow(
		child, 0, 1, 2*time.Second, 20*time.Millisecond,
	)
	if err == nil || !strings.Contains(err.Error(), "want exactly 1") {
		t.Fatalf("collect extra Oracle request-window call error = %v", err)
	}
}

func newDifferentialOracleRecordTestRuntime(t *testing.T) (*differentialChild, string) {
	t.Helper()
	root := t.TempDir()
	runtimePath := filepath.Join(root, "fake-container")
	script := `#!/bin/sh
for last_arg do :; done
index=${last_arg##*.}
record="$FAKE_ORACLE_RECORD_DIR/$index"
case "$*" in
  *sysread*)
    [ -f "$record" ] || exit 1
    /bin/cat "$record"
    ;;
  *)
    if [ "${FAKE_ORACLE_HIDE_ZERO_ONCE:-}" = 1 ] && [ "$index" = 0 ] && [ ! -f "$FAKE_ORACLE_RECORD_DIR/.zero-seen" ]; then
      : > "$FAKE_ORACLE_RECORD_DIR/.zero-seen"
      exit 1
    fi
    [ -f "$record" ]
    ;;
esac
`
	if err := os.WriteFile(runtimePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_ORACLE_RECORD_DIR", root)
	return &differentialChild{runtime: runtimePath, name: "fake-oracle"}, root
}

func writeDifferentialOracleRecordTestFile(t *testing.T, root string, index int, raw string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, fmt.Sprint(index)), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProbeDifferentialFixtureLoopbackOwnershipRejectsForeignListener(t *testing.T) {
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer foreign.Close()
	port := foreign.Listener.Addr().(*net.TCPAddr).Port

	if err := probeDifferentialFixtureLoopback(port, "expected-owner-token"); err == nil {
		t.Fatal("foreign loopback listener was accepted as the differential fixture")
	}
}

func TestDifferentialFixtureOwnsLoopbackPortWithoutRecordingProbe(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{
		Name: "primary",
		Response: DifferentialFixtureResponse{
			Status: http.StatusOK,
			Body:   "fixture",
		},
	})
	if err != nil {
		t.Fatalf("newDifferentialFixture() error = %v", err)
	}
	defer fixture.close()

	if err := probeDifferentialFixtureLoopback(fixture.port(), fixture.probeToken); err != nil {
		t.Fatalf("probe owned fixture: %v", err)
	}
	select {
	case request := <-fixture.requests:
		t.Fatalf("fixture ownership probe was recorded as a behavior request: %#v", request)
	default:
	}
}

func TestDifferentialFixtureDefaultsToHTTPWireProtocol(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{
		Name: "primary",
		Response: DifferentialFixtureResponse{
			Status: http.StatusCreated,
			Body:   "created",
		},
		ExpectedCalls: 1,
	})
	if err != nil {
		t.Fatalf("newDifferentialFixture() error = %v", err)
	}
	defer fixture.close()

	request, err := http.NewRequest(
		http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/default?wire=http", fixture.port()),
		strings.NewReader("request body"),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "upstream.example.test"
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request default HTTP fixture: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close default HTTP response: %v / %v", readErr, closeErr)
	}
	if response.StatusCode != http.StatusCreated || string(body) != "created" {
		t.Fatalf("default HTTP response = %d/%q", response.StatusCode, body)
	}
	captured, err := fixture.collect(1)
	if err != nil {
		t.Fatalf("collect default HTTP fixture: %v", err)
	}
	if len(captured) != 1 || captured[0].Method != http.MethodPost ||
		captured[0].Path != "/default?wire=http" ||
		captured[0].Host != "upstream.example.test" || captured[0].Body != "request body" {
		t.Fatalf("default HTTP capture = %#v", captured)
	}
}

func TestDifferentialHTTPTCPFixtureCapturesHTTPAndCompleteJSONWithoutEOF(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{
		Name: "origin-and-tcp-log", WireProtocol: differentialFixtureWireHTTPTCP,
		ExpectedCalls: 2,
		Response:      DifferentialFixtureResponse{Status: http.StatusCreated, Body: "origin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.close()

	response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/origin", fixture.port()))
	if err != nil {
		t.Fatalf("request HTTP/TCP origin: %v", err)
	}
	_ = response.Body.Close()
	connection, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.port())))
	if err != nil {
		t.Fatalf("dial HTTP/TCP raw sink: %v", err)
	}
	defer func() { _ = connection.Close() }()
	if _, err := io.WriteString(connection, `{"case":"tcp","status":201}`); err != nil {
		t.Fatalf("write complete JSON frame: %v", err)
	}

	captured, err := fixture.collectWithTimeout(2, time.Second)
	if err != nil {
		t.Fatalf("collect HTTP/TCP fixture without closing raw writer: %v", err)
	}
	if len(captured) != 2 || captured[0].Method != http.MethodGet || captured[0].Path != "/origin" {
		t.Fatalf("HTTP/TCP HTTP capture = %#v", captured)
	}
	if raw := captured[1]; raw.Method != "TCP" || raw.Path != "" || raw.Host != "" ||
		len(raw.Headers) != 0 || raw.Body != `{"case":"tcp","status":201}` {
		t.Fatalf("HTTP/TCP raw capture = %#v", raw)
	}
}

func TestDifferentialHTTPUDPFixturePreservesDatagramBoundariesAndExcludesDatadogOrigin(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{
		Name: "datadog-http-origin-and-udp-sink", WireProtocol: differentialFixtureWireHTTPUDP,
		ExpectedCalls: 6, CaptureAllCalls: true, OmitHTTPOriginCall: true,
		Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "origin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.close()

	response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/origin", fixture.port()))
	if err != nil {
		t.Fatalf("request HTTP/UDP origin: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(body) != "origin" {
		t.Fatalf("HTTP/UDP origin response = %q, %v", body, err)
	}
	connection, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.port())))
	if err != nil {
		t.Fatalf("dial HTTP/UDP sink: %v", err)
	}
	defer func() { _ = connection.Close() }()
	for index := range 6 {
		if _, err := fmt.Fprintf(connection, "metric.%d:1|c", index); err != nil {
			t.Fatalf("write datagram %d: %v", index, err)
		}
	}

	captured, err := fixture.collectWithTimeout(6, time.Second)
	if err != nil {
		t.Fatalf("collect six UDP datagrams: %v", err)
	}
	for index, request := range captured {
		if request.Method != "UDP" || request.Path != "" || request.Host != "" ||
			len(request.Headers) != 0 || request.Body != fmt.Sprintf("metric.%d:1|c", index) {
			t.Fatalf("UDP datagram %d = %#v", index, request)
		}
	}
}

func TestDifferentialFixtureRejectsOmittedHTTPOriginForNonHTTPWire(t *testing.T) {
	for _, wire := range []string{"", differentialFixtureWireTLSTCP, differentialFixtureWireT1KV2} {
		spec := DifferentialFixture{
			Name: "invalid-origin-contract", WireProtocol: wire, OmitHTTPOriginCall: true,
		}
		_, err := startDifferentialFixture(spec)
		if err == nil || !strings.Contains(err.Error(), "omit_http_origin_call is unsupported") {
			t.Fatalf("wire %q error = %v", wire, err)
		}
		_, err = prepareDifferentialOracleFixtureLaunch(spec)
		if err == nil || !strings.Contains(err.Error(), "omit_http_origin_call is unsupported") {
			t.Fatalf("Oracle wire %q error = %v", wire, err)
		}
	}
}

func TestDifferentialHTTPUDPFixtureCapturesOrdinaryHTTPOriginAndUDPLog(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{
		Name: "origin-and-udp-log", WireProtocol: differentialFixtureWireHTTPUDP,
		ExpectedCalls: 2, Response: DifferentialFixtureResponse{Status: http.StatusNoContent},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.close()

	response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/origin", fixture.port()))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	connection, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.port())))
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := io.WriteString(connection, `{"case":"udp"}`)
	_ = connection.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	captured, err := fixture.collectWithTimeout(2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if captured[0].Method != http.MethodGet || captured[1].Method != "UDP" {
		t.Fatalf("ordinary HTTP/UDP capture = %#v", captured)
	}
}

func TestDifferentialRawFixtureResetAndCloseDoNotLeakCallsOrListeners(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{
		Name: "reset-http-udp", WireProtocol: differentialFixtureWireHTTPUDP,
		Response: DifferentialFixtureResponse{Status: http.StatusNoContent},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fixture.close)
	port := fixture.port()
	response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/stale", port))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	fixture.reset()

	connection, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := io.WriteString(connection, "fresh")
	_ = connection.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	captured, err := fixture.collectWithTimeout(1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(captured) != 1 || captured[0].Method != "UDP" || captured[0].Body != "fresh" {
		t.Fatalf("post-reset capture = %#v", captured)
	}
	fixture.close()
	closed, err := net.DialTimeout(
		"tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 100*time.Millisecond,
	)
	if err == nil {
		_ = closed.Close()
		t.Fatal("closed raw fixture still accepts TCP connections")
	}
}

func TestDifferentialTLSTCPFixtureCapturesNewlineFrameWithoutEOF(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{
		Name: "sls-logger-tls", WireProtocol: differentialFixtureWireTLSTCP, ExpectedCalls: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.close()

	connection, err := tls.Dial(
		"tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(fixture.port())),
		&tls.Config{ //nolint:gosec // deterministic self-signed test fixture
			InsecureSkipVerify: true,
		},
	)
	if err != nil {
		t.Fatalf("dial TLS/TCP fixture: %v", err)
	}
	defer func() { _ = connection.Close() }()
	const frame = "<14>1 2026-08-29T00:00:00Z host APISIX - - - {\"case\":\"sls\"}\n"
	if _, err := io.WriteString(connection, frame); err != nil {
		t.Fatalf("write TLS/TCP frame: %v", err)
	}
	captured, err := fixture.collectWithTimeout(1, time.Second)
	if err != nil {
		t.Fatalf("collect TLS/TCP fixture without closing raw writer: %v", err)
	}
	if len(captured) != 1 || captured[0].Method != "TCP" || captured[0].Path != "" ||
		captured[0].Host != "" || len(captured[0].Headers) != 0 || captured[0].Body != frame {
		t.Fatalf("TLS/TCP raw capture = %#v", captured)
	}
}

func TestDifferentialOracleRawFixtureRecordRoundTripFailsClosed(t *testing.T) {
	const body = "<14>1 fixture\n"
	raw := encodeDifferentialOracleRawFixtureRecord("TCP", []byte(body))
	captured, err := parseDifferentialOracleFixtureRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if captured.Method != "TCP" || captured.Path != "" || captured.Host != "" ||
		len(captured.Headers) != 0 || captured.Body != body {
		t.Fatalf("raw Oracle record = %#v", captured)
	}
	for _, malformed := range [][]byte{
		[]byte("APISIX-GO-DIFFERENTIAL-RAW/1 TCP 100\nshort"),
		append(raw, 'x'),
		[]byte("APISIX-GO-DIFFERENTIAL-RAW/1 SCTP 0\n"),
	} {
		if _, err := parseDifferentialOracleFixtureRequest(malformed); err == nil {
			t.Fatalf("malformed raw Oracle record was accepted: %q", malformed)
		}
	}
}

func TestDifferentialT1KV2FixtureCapturesEmbeddedHTTPAndWritesPinnedReject(t *testing.T) {
	fixture, err := newDifferentialFixture(DifferentialFixture{
		Name:          "waf",
		WireProtocol:  differentialFixtureWireT1KV2,
		ExpectedCalls: 1,
	})
	if err != nil {
		t.Fatalf("newDifferentialFixture() error = %v", err)
	}
	defer fixture.close()

	connection, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", fixture.port()))
	if err != nil {
		t.Fatalf("dial T1K fixture: %v", err)
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set T1K fixture deadline: %v", err)
	}

	requestHead := "POST /orders?debug=1 HTTP/1.1\r\n" +
		"Host: gateway.example.test\r\n" +
		"Content-Length: 12\r\n" +
		"Content-Type: application/x-www-form-urlencoded\r\n\r\n"
	for _, frame := range []differentialT1KTestFrame{
		{tag: 0x41, payload: requestHead},
		{tag: 0x02, payload: "request body"},
		{tag: 0x20, payload: "Proto:2\n"},
		{tag: 0x83, payload: "RemoteAddr:198.51.100.2\nServerName:gateway.example.test\n"},
	} {
		if err := writeDifferentialT1KTestFrame(connection, frame); err != nil {
			t.Fatalf("write T1K request frame: %v", err)
		}
	}

	frames, err := readDifferentialT1KTestFrames(connection)
	if err != nil {
		select {
		case fixtureErr := <-fixture.errors:
			t.Fatalf("read T1K response: %v; fixture: %v", err, fixtureErr)
		default:
			t.Fatalf("read T1K response: %v", err)
		}
	}
	want := []differentialT1KTestFrame{
		{tag: 0x41, payload: "?"},
		{tag: 0x02, payload: "403"},
		{tag: 0x25, payload: `{"event_id":"b3c6ce574dc24f09a01f634a39dca83b","request_hit_whitelist":false}`},
		{tag: 0x23, payload: "Set-Cookie:sl-session=ulgbPfMSuWRNsi/u7Aj9aA==; Domain=; Path=/; Max-Age=86400\n"},
		{tag: 0xa4, payload: "<!-- event_id: b3c6ce574dc24f09a01f634a39dca83b -->"},
	}
	if fmt.Sprint(frames) != fmt.Sprint(want) {
		t.Fatalf("T1K response frames = %#v, want %#v", frames, want)
	}

	captured, err := fixture.collect(1)
	if err != nil {
		t.Fatalf("collect T1K fixture: %v", err)
	}
	if len(captured) != 1 || captured[0].Method != http.MethodPost ||
		captured[0].Path != "/orders?debug=1" ||
		captured[0].Host != "gateway.example.test" || captured[0].Body != "request body" {
		t.Fatalf("T1K embedded HTTP capture = %#v", captured)
	}
}

func TestReadDifferentialT1KV2RequestRejectsMalformedFrames(t *testing.T) {
	validHead := "GET /hello HTTP/1.1\r\nHost: gateway.example.test\r\n\r\n"
	validExtra := "RemoteAddr:198.51.100.2\n"
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "missing FIRST", raw: differentialT1KTestBytes(
			differentialT1KTestFrame{tag: 0x01, payload: validHead},
		)},
		{name: "LAST before EXTRA", raw: differentialT1KTestBytes(
			differentialT1KTestFrame{tag: 0xc1, payload: validHead},
		)},
		{name: "FIRST repeated", raw: differentialT1KTestBytes(
			differentialT1KTestFrame{tag: 0x41, payload: validHead},
			differentialT1KTestFrame{tag: 0x60, payload: "Proto:2\n"},
		)},
		{name: "BODY after VERSION", raw: differentialT1KTestBytes(
			differentialT1KTestFrame{tag: 0x41, payload: validHead},
			differentialT1KTestFrame{tag: 0x20, payload: "Proto:2\n"},
			differentialT1KTestFrame{tag: 0x02, payload: "late"},
		)},
		{name: "duplicate BODY", raw: differentialT1KTestBytes(
			differentialT1KTestFrame{tag: 0x41, payload: validHead},
			differentialT1KTestFrame{tag: 0x02, payload: "one"},
			differentialT1KTestFrame{tag: 0x02, payload: "two"},
		)},
		{name: "unknown tag", raw: differentialT1KTestBytes(
			differentialT1KTestFrame{tag: 0x41, payload: validHead},
			differentialT1KTestFrame{tag: 0x04, payload: "unknown"},
		)},
		{name: "wrong VERSION payload", raw: differentialT1KTestBytes(
			differentialT1KTestFrame{tag: 0x41, payload: validHead},
			differentialT1KTestFrame{tag: 0x20, payload: "Proto:1\n"},
		)},
		{name: "EXTRA without LAST", raw: differentialT1KTestBytes(
			differentialT1KTestFrame{tag: 0x41, payload: validHead},
			differentialT1KTestFrame{tag: 0x20, payload: "Proto:2\n"},
			differentialT1KTestFrame{tag: 0x03, payload: validExtra},
		)},
		{name: "truncated header", raw: []byte{0x41, 0x01, 0x00}},
		{name: "truncated payload", raw: []byte{0x41, 0x04, 0x00, 0x00, 0x00, 'G'}},
		{name: "oversized payload", raw: func() []byte {
			header := []byte{0x41, 0, 0, 0, 0}
			binary.LittleEndian.PutUint32(header[1:], differentialT1KMaxFramePayload+1)
			return header
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := readDifferentialT1KV2Request(bytes.NewReader(test.raw)); err == nil {
				t.Fatal("readDifferentialT1KV2Request() error = nil")
			}
		})
	}
}

func TestDifferentialOracleFixtureProgramsCompile(t *testing.T) {
	for _, test := range []struct {
		name    string
		program string
	}{
		{name: "default HTTP", program: differentialOracleHTTPFixtureProgram()},
		{name: "HTTP and TCP", program: differentialOracleHTTPTCPFixtureProgram()},
		{name: "HTTP and UDP", program: differentialOracleHTTPUDPFixtureProgram()},
		{name: "T1K v2", program: differentialOracleT1KV2FixtureProgram()},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command("perl", "-MIO::Socket::INET", "-c", "-e", test.program)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("compile Oracle fixture program: %v\n%s", err, output)
			}
		})
	}
}

func TestDifferentialOracleTLSTCPFixtureProgramCompilesWithoutOptionalPerlSSLModules(t *testing.T) {
	command := exec.Command(
		"perl", "-MIO::Socket::INET", "-c", "-e",
		differentialOracleTLSTCPFixtureProgram(),
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile Oracle TLS/TCP fixture program: %v\n%s", err, output)
	}
}

func TestDifferentialOracleOpenSSLTLSTCPFixtureCapturesOneNewlineFrame(t *testing.T) {
	opensslPath := differentialOpenSSLServerForTest(t)
	port := reserveDifferentialFixturePortForTest(t)
	directory := t.TempDir()
	ready := filepath.Join(directory, "ready")
	recordBase := filepath.Join(directory, "request.raw")
	command, output := startDifferentialOracleProgramForTest(
		t,
		renderDifferentialOracleTLSTCPFixtureProgramWithOpenSSL(
			port, ready, recordBase, opensslPath,
		),
		"APISIX_GO_FIXTURE_CERTIFICATE_HEX="+hex.EncodeToString([]byte(differentialFixtureCertificatePEM)),
		"APISIX_GO_FIXTURE_PRIVATE_KEY_HEX="+hex.EncodeToString([]byte(differentialFixturePrivateKeyPEM)),
	)
	waitDifferentialFixtureFileForTest(t, ready, output)

	connection, err := tls.Dial(
		"tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		&tls.Config{
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
		}, //nolint:gosec // self-signed deterministic fixture
	)
	if err != nil {
		t.Fatalf("dial Oracle openssl TLS fixture: %v; process=%s", err, output.String())
	}
	defer func() { _ = connection.Close() }()
	const frame = "<14>1 2026-08-29T00:00:00Z host APISIX - - - {\"case\":\"openssl\"}\n"
	if _, err := io.WriteString(connection, frame); err != nil {
		t.Fatal(err)
	}
	waitDifferentialFixtureFileForTest(t, recordBase+".0", output)
	raw, err := os.ReadFile(recordBase + ".0")
	if err != nil {
		t.Fatal(err)
	}
	captured, err := parseDifferentialOracleFixtureRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if captured.Method != "TCP" || captured.Path != "" || captured.Host != "" ||
		len(captured.Headers) != 0 || captured.Body != frame {
		t.Fatalf("Oracle openssl TLS capture = %#v", captured)
	}
	_ = command
}

func TestDifferentialOracleOpenSSLTLSTCPFixtureRejectsExtraAndTruncatedFrames(t *testing.T) {
	opensslPath := differentialOpenSSLServerForTest(t)
	tests := []struct {
		name      string
		payload   string
		wantError string
	}{
		{name: "extra data", payload: "first\nsecond", wantError: "extra TLS frame data"},
		{name: "truncated", payload: "missing newline", wantError: "truncated TLS frame"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			port := reserveDifferentialFixturePortForTest(t)
			directory := t.TempDir()
			ready := filepath.Join(directory, "ready")
			recordBase := filepath.Join(directory, "request.raw")
			command := exec.Command(
				"perl", "-MIO::Socket::INET", "-e",
				renderDifferentialOracleTLSTCPFixtureProgramWithOpenSSL(
					port, ready, recordBase, opensslPath,
				),
			)
			var output bytes.Buffer
			command.Stdout = &output
			command.Stderr = &output
			command.Env = append(
				os.Environ(),
				"APISIX_GO_FIXTURE_CERTIFICATE_HEX="+hex.EncodeToString([]byte(differentialFixtureCertificatePEM)),
				"APISIX_GO_FIXTURE_PRIVATE_KEY_HEX="+hex.EncodeToString([]byte(differentialFixturePrivateKeyPEM)),
			)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if command.ProcessState == nil {
					_ = command.Process.Kill()
					_ = command.Wait()
				}
			})
			waitDifferentialFixtureFileForTest(t, ready, &output)
			connection, err := tls.Dial(
				"tcp",
				net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
				&tls.Config{
					InsecureSkipVerify: true,
					MinVersion:         tls.VersionTLS12,
				}, //nolint:gosec // self-signed deterministic fixture
			)
			if err != nil {
				t.Fatalf("dial Oracle openssl TLS fixture: %v; process=%s", err, output.String())
			}
			if _, err := io.WriteString(connection, test.payload); err != nil {
				t.Fatal(err)
			}
			_ = connection.Close()
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(output.String(), test.wantError) {
					t.Fatalf("malformed TLS fixture result = %v, output=%s", err, output.String())
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("malformed TLS fixture did not fail closed: %s", output.String())
			}
			if _, err := os.Stat(recordBase + ".0"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("malformed TLS frame produced a record: %v", err)
			}
		})
	}
}

func differentialOpenSSLServerForTest(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl is unavailable")
	}
	command := exec.Command(path, "s_server", "-help")
	output, _ := command.CombinedOutput()
	if !bytes.Contains(output, []byte("-naccept")) {
		t.Skipf("openssl %s does not support s_server -naccept", path)
	}
	return path
}

func TestPrepareDifferentialOracleRawFixtureLaunches(t *testing.T) {
	tests := []struct {
		wire         string
		omitHTTP     bool
		wantCapture  bool
		wantTLSCreds bool
	}{
		{wire: differentialFixtureWireHTTPTCP, wantCapture: true},
		{wire: differentialFixtureWireHTTPUDP, omitHTTP: true},
		{wire: differentialFixtureWireTLSTCP, wantCapture: true, wantTLSCreds: true},
	}
	for _, test := range tests {
		t.Run(test.wire, func(t *testing.T) {
			fixture := DifferentialFixture{
				Name: "raw", WireProtocol: test.wire, OmitHTTPOriginCall: test.omitHTTP,
				Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "origin"},
			}
			launch, err := prepareDifferentialOracleFixtureLaunch(fixture)
			if err != nil {
				t.Fatal(err)
			}
			if launch.program == "" || launch.captureHTTP != test.wantCapture ||
				(launch.certificateHex != "") != test.wantTLSCreds ||
				(launch.privateKeyHex != "") != test.wantTLSCreds {
				t.Fatalf("Oracle %s launch = %#v", test.wire, launch)
			}
			if !differentialOracleBootstrapFixture(fixture) {
				t.Fatalf("Oracle %s fixture is not bootstrapped", test.wire)
			}
		})
	}
}

func TestDifferentialOracleHTTPTCPFixtureCapturesRenderedHTTPAndRawFrame(t *testing.T) {
	port := reserveDifferentialFixturePortForTest(t)
	directory := t.TempDir()
	ready := filepath.Join(directory, "ready")
	recordBase := filepath.Join(directory, "request.raw")
	response, err := renderDifferentialOracleFixtureResponse(DifferentialFixtureResponse{
		Status: http.StatusOK, Headers: map[string]string{"X-Fixture": "oracle"}, Body: "origin",
	})
	if err != nil {
		t.Fatal(err)
	}
	command, output := startDifferentialOracleProgramForTest(
		t,
		renderDifferentialOracleHTTPTCPFixtureProgram(port, ready, recordBase),
		"APISIX_GO_FIXTURE_RESPONSE_HEX="+hex.EncodeToString(response),
		"APISIX_GO_FIXTURE_CAPTURE_HTTP=true",
	)
	waitDifferentialFixtureFileForTest(t, ready, output)

	request, err := http.NewRequest(
		http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/origin?source=oracle", port),
		strings.NewReader("body"),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "origin.example.test"
	request.Header.Set("X-Rendered", "yes")
	httpResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request Oracle HTTP/TCP origin: %v; process=%s", err, output.String())
	}
	_ = httpResponse.Body.Close()
	connection, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if _, err := io.WriteString(connection, `{"case":"oracle-tcp"}`); err != nil {
		t.Fatal(err)
	}
	waitDifferentialFixtureFileForTest(t, recordBase+".1", output)

	httpRaw, err := os.ReadFile(recordBase + ".0")
	if err != nil {
		t.Fatal(err)
	}
	httpCaptured, err := parseDifferentialOracleFixtureRequest(httpRaw)
	if err != nil {
		t.Fatal(err)
	}
	if httpCaptured.Method != http.MethodPost || httpCaptured.Path != "/origin?source=oracle" ||
		httpCaptured.Host != "origin.example.test" || httpCaptured.Headers.Get("X-Rendered") != "yes" ||
		httpCaptured.Body != "body" {
		t.Fatalf("Oracle rendered HTTP capture = %#v", httpCaptured)
	}
	raw, err := os.ReadFile(recordBase + ".1")
	if err != nil {
		t.Fatal(err)
	}
	rawCaptured, err := parseDifferentialOracleFixtureRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if rawCaptured.Method != "TCP" || rawCaptured.Body != `{"case":"oracle-tcp"}` {
		t.Fatalf("Oracle TCP capture = %#v", rawCaptured)
	}
	_ = command
}

func TestDifferentialOracleHTTPUDPFixtureOmitsOriginAndPreservesSixDatagrams(t *testing.T) {
	port := reserveDifferentialFixturePortForTest(t)
	directory := t.TempDir()
	ready := filepath.Join(directory, "ready")
	recordBase := filepath.Join(directory, "request.raw")
	response, err := renderDifferentialOracleFixtureResponse(DifferentialFixtureResponse{
		Status: http.StatusOK, Body: "origin",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, output := startDifferentialOracleProgramForTest(
		t,
		renderDifferentialOracleHTTPUDPFixtureProgram(port, ready, recordBase),
		"APISIX_GO_FIXTURE_RESPONSE_HEX="+hex.EncodeToString(response),
		"APISIX_GO_FIXTURE_CAPTURE_HTTP=false",
	)
	waitDifferentialFixtureFileForTest(t, ready, output)
	httpResponse, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/origin", port))
	if err != nil {
		t.Fatalf("request Oracle HTTP/UDP origin: %v; process=%s", err, output.String())
	}
	_ = httpResponse.Body.Close()
	connection, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	for index := range 6 {
		if _, err := fmt.Fprintf(connection, "metric.%d:1|c", index); err != nil {
			t.Fatal(err)
		}
	}
	waitDifferentialFixtureFileForTest(t, recordBase+".5", output)
	for index := range 6 {
		raw, err := os.ReadFile(fmt.Sprintf("%s.%d", recordBase, index))
		if err != nil {
			t.Fatal(err)
		}
		captured, err := parseDifferentialOracleFixtureRequest(raw)
		if err != nil {
			t.Fatal(err)
		}
		if captured.Method != "UDP" || captured.Body != fmt.Sprintf("metric.%d:1|c", index) {
			t.Fatalf("Oracle UDP datagram %d = %#v", index, captured)
		}
	}
	if _, err := os.Stat(recordBase + ".6"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Oracle HTTP origin polluted UDP calls: %v", err)
	}
}

func reserveDifferentialFixturePortForTest(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func startDifferentialOracleProgramForTest(
	t *testing.T,
	program string,
	environment ...string,
) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	command := exec.Command("perl", "-MIO::Socket::INET", "-e", program)
	output := &bytes.Buffer{}
	command.Stdout = output
	command.Stderr = output
	command.Env = append(os.Environ(), environment...)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	return command, output
}

func waitDifferentialFixtureFileForTest(t *testing.T, path string, output *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixture file %s did not appear: %s", path, output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDifferentialOracleBootstrapFixtureOptInRequiresHTTPAndRequestWindow(t *testing.T) {
	tests := []struct {
		name    string
		fixture DifferentialFixture
		want    bool
	}{
		{name: "ordinary HTTP", fixture: DifferentialFixture{}},
		{
			name: "request-window HTTP",
			fixture: DifferentialFixture{
				ExpectedCalls: 1, CollectTimeoutMillis: 200,
				RequestWindowQuietMillis: 50,
			},
			want: true,
		},
		{
			name: "request-window T1K",
			fixture: DifferentialFixture{
				WireProtocol: differentialFixtureWireT1KV2, ExpectedCalls: 1,
				CollectTimeoutMillis: 200, RequestWindowQuietMillis: 50,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := differentialOracleBootstrapFixture(test.fixture); got != test.want {
				t.Fatalf("differentialOracleBootstrapFixture() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestDifferentialOracleRunArgsKeepDefaultEntrypointWithoutBootstrap(t *testing.T) {
	t.Setenv(differentialHostGatewayEnv, "")
	identity := OracleIdentity{
		ImageRepository: "example.test/apisix",
		ImageLinuxAMD64: "sha256:deadbeef",
	}
	got, err := differentialOracleRunArgs(
		identity,
		"ordinary-oracle",
		"/host/config.yaml",
		"/host/apisix.yaml",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"run", "--rm", "--detach", "--name", "ordinary-oracle", "--pull=never",
		"--platform", "linux/amd64", "--network", "bridge",
		"--env", "APISIX_STAND_ALONE=true",
		"--volume", "/host/config.yaml:/usr/local/apisix/conf/config.yaml:ro",
		"--volume", "/host/apisix.yaml:/usr/local/apisix/conf/apisix.yaml:ro",
		"example.test/apisix@sha256:deadbeef",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ordinary Oracle run args = %v, want %v", got, want)
	}
}

func TestDifferentialOracleRunArgsPinExplicitHostGateway(t *testing.T) {
	t.Setenv(differentialHostGatewayEnv, "192.168.127.1")
	identity := OracleIdentity{
		ImageRepository: "example.test/apisix",
		ImageLinuxAMD64: "sha256:deadbeef",
	}
	got, err := differentialOracleRunArgs(
		identity,
		"host-routed-oracle",
		"/host/config.yaml",
		"/host/apisix.yaml",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "--add-host\nhost.containers.internal:192.168.127.1") {
		t.Fatalf("Oracle run args %q do not pin the explicit host gateway", joined)
	}
}

func TestDifferentialOracleRunArgsRejectInvalidHostGateway(t *testing.T) {
	t.Setenv(differentialHostGatewayEnv, "not-an-ip")
	identity := OracleIdentity{
		ImageRepository: "example.test/apisix",
		ImageLinuxAMD64: "sha256:deadbeef",
	}
	if _, err := differentialOracleRunArgs(
		identity,
		"invalid-host-gateway-oracle",
		"/host/config.yaml",
		"/host/apisix.yaml",
		nil,
	); err == nil || !strings.Contains(err.Error(), differentialHostGatewayEnv) {
		t.Fatalf("differentialOracleRunArgs() error = %v, want invalid %s error", err, differentialHostGatewayEnv)
	}
}

func TestDifferentialOracleRunArgsBootstrapFixtureBeforePinnedEntrypoint(t *testing.T) {
	t.Setenv(differentialHostGatewayEnv, "")
	fixture := DifferentialFixture{
		ExpectedCalls: 1, CollectTimeoutMillis: 200, RequestWindowQuietMillis: 50,
		Response: DifferentialFixtureResponse{Status: http.StatusNoContent},
	}
	launch, err := prepareDifferentialOracleFixtureLaunch(fixture)
	if err != nil {
		t.Fatal(err)
	}
	identity := OracleIdentity{
		ImageRepository: "example.test/apisix",
		ImageLinuxAMD64: "sha256:deadbeef",
	}
	args, err := differentialOracleRunArgs(
		identity,
		"bootstrap-oracle",
		"/host/config.yaml",
		"/host/apisix.yaml",
		&launch,
	)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\n")
	for _, required := range []string{
		"--entrypoint\nperl",
		"APISIX_GO_FIXTURE_RESPONSE_HEX=" + launch.responseHex,
		"APISIX_GO_FIXTURE_DELAY_MILLIS=0",
		"example.test/apisix@sha256:deadbeef\n-MIO::Socket::INET\n-e",
		"/docker-entrypoint.sh",
		"docker-start",
		differentialOracleFixtureReady,
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("bootstrapped Oracle run args %q do not contain %q", joined, required)
		}
	}
}

func TestDifferentialOracleBootstrapStartsFixtureBeforeEntrypoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	ready := filepath.Join(directory, "ready")
	marker := filepath.Join(directory, "entrypoint-started")
	script := filepath.Join(directory, "entrypoint.sh")
	scriptBody := "#!/bin/sh\n" +
		"[ -f \"$BOOTSTRAP_READY\" ] || exit 41\n" +
		"perl -MIO::Socket::INET -e 'my $s = IO::Socket::INET->new(PeerAddr => \"127.0.0.1\", PeerPort => $ARGV[0], Proto => \"tcp\") or exit 42; close($s);' \"$BOOTSTRAP_PORT\" || exit $?\n" +
		"printf started > \"$BOOTSTRAP_MARKER\"\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o700); err != nil {
		t.Fatal(err)
	}
	fixtureProgram := fmt.Sprintf(
		`my $listener = IO::Socket::INET->new(LocalAddr => "127.0.0.1", LocalPort => %d, Proto => "tcp", Listen => 1, ReuseAddr => 1) or die "listen: $!\n"; open(my $ready_file, ">", "%s") or die "ready: $!\n"; print {$ready_file} "ready\n"; close($ready_file); my $client = $listener->accept() or die "accept: $!\n"; close($client); close($listener);`,
		port,
		ready,
	)
	program := renderDifferentialOracleBootstrapProgram(
		fixtureProgram, ready, "/bin/sh", script,
	)
	command := exec.Command("perl", "-MIO::Socket::INET", "-e", program)
	command.Env = append(
		os.Environ(),
		"BOOTSTRAP_READY="+ready,
		"BOOTSTRAP_PORT="+strconv.Itoa(port),
		"BOOTSTRAP_MARKER="+marker,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run Oracle bootstrap: %v\n%s", err, output)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "started" {
		t.Fatalf("bootstrap entrypoint marker = %q, %v", data, err)
	}
}

func TestDifferentialOracleBootstrapDoesNotStartEntrypointWhenFixtureBindFails(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port
	directory := t.TempDir()
	ready := filepath.Join(directory, "ready")
	marker := filepath.Join(directory, "entrypoint-started")
	script := filepath.Join(directory, "entrypoint.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf started > \"$BOOTSTRAP_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	fixtureProgram := fmt.Sprintf(
		`my $listener = IO::Socket::INET->new(LocalAddr => "127.0.0.1", LocalPort => %d, Proto => "tcp", Listen => 1, ReuseAddr => 1) or die "listen: $!\n"; open(my $ready_file, ">", "%s") or die "ready: $!\n"; close($ready_file);`,
		port,
		ready,
	)
	program := renderDifferentialOracleBootstrapProgram(
		fixtureProgram, ready, "/bin/sh", script,
	)
	command := exec.Command("perl", "-MIO::Socket::INET", "-e", program)
	command.Env = append(os.Environ(), "BOOTSTRAP_MARKER="+marker)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "fixture exited before ready") {
		t.Fatalf("run bind-failing Oracle bootstrap = %v\n%s", err, output)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bootstrap entrypoint started after fixture failure: %v", err)
	}
}

func TestEnsureDifferentialOracleFixtureSkipsAlreadyBootstrappedFixture(t *testing.T) {
	child := &differentialChild{oracleFixtureBootstrapped: true}
	fixture := DifferentialFixture{
		ExpectedCalls: 1, CollectTimeoutMillis: 200, RequestWindowQuietMillis: 50,
	}
	if err := ensureDifferentialOracleFixture(child, fixture); err != nil {
		t.Fatalf("ensure bootstrapped Oracle fixture: %v", err)
	}
}

func TestDifferentialOracleHTTPFixtureDelaysResponseAfterCapture(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve Oracle fixture port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release Oracle fixture port: %v", err)
	}

	directory := t.TempDir()
	ready := filepath.Join(directory, "ready")
	recordBase := filepath.Join(directory, "request.raw")
	program := renderDifferentialOracleHTTPFixtureProgram(port, ready, recordBase)
	responseBytes, err := renderDifferentialOracleFixtureResponse(DifferentialFixtureResponse{
		Status: http.StatusOK,
		Body:   "oracle delayed response",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("perl", "-MIO::Socket::INET", "-e", program)
	var processOutput bytes.Buffer
	command.Stdout = &processOutput
	command.Stderr = &processOutput
	command.Env = append(
		os.Environ(),
		"APISIX_GO_FIXTURE_RESPONSE_HEX="+hex.EncodeToString(responseBytes),
		"APISIX_GO_FIXTURE_DELAY_MILLIS=100",
	)
	if err := command.Start(); err != nil {
		t.Fatalf("start Oracle HTTP fixture: %v", err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Oracle HTTP fixture did not become ready: %s", processOutput.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	started := time.Now()
	response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/oracle-delay", port))
	if err != nil {
		t.Fatalf("request Oracle HTTP fixture: %v; process=%s", err, processOutput.String())
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close Oracle HTTP fixture response: %v / %v", readErr, closeErr)
	}
	if response.StatusCode != http.StatusOK || string(body) != "oracle delayed response" {
		t.Fatalf("Oracle HTTP fixture response = %d/%q", response.StatusCode, body)
	}
	if elapsed := time.Since(started); elapsed < 80*time.Millisecond {
		t.Fatalf("Oracle fixture response delay = %s, want at least 80ms", elapsed)
	}
	if _, err := os.Stat(recordBase + ".0"); err != nil {
		t.Fatalf("Oracle HTTP fixture did not capture before responding: %v", err)
	}
}

func TestDifferentialOracleT1KV2FixtureCapturesEmbeddedHTTP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve Oracle fixture port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release Oracle fixture port: %v", err)
	}

	directory := t.TempDir()
	ready := filepath.Join(directory, "ready")
	recordBase := filepath.Join(directory, "request.raw")
	program := renderDifferentialOracleT1KV2FixtureProgram(port, ready, recordBase)
	command := exec.Command("perl", "-MIO::Socket::INET", "-e", program)
	var processOutput bytes.Buffer
	command.Stdout = &processOutput
	command.Stderr = &processOutput
	command.Env = append(
		os.Environ(),
		"APISIX_GO_FIXTURE_RESPONSE_HEX="+hex.EncodeToString(differentialT1KV2RejectResponse()),
	)
	if err := command.Start(); err != nil {
		t.Fatalf("start Oracle T1K fixture: %v", err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Oracle T1K fixture did not become ready: %s", processOutput.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	connection, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
	if err != nil {
		t.Fatalf("dial Oracle T1K fixture: %v", err)
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set Oracle T1K fixture deadline: %v", err)
	}
	for _, frame := range []differentialT1KTestFrame{
		{
			tag: 0x41,
			payload: "POST /oracle?wire=t1k HTTP/1.1\r\n" +
				"Host: gateway.example.test\r\nContent-Length: 4\r\n\r\n",
		},
		{tag: 0x02, payload: "body"},
		{tag: 0x20, payload: "Proto:2\n"},
		{tag: 0x83, payload: "RemoteAddr:198.51.100.2\n"},
	} {
		if err := writeDifferentialT1KTestFrame(connection, frame); err != nil {
			t.Fatalf("write Oracle T1K request: %v", err)
		}
	}
	frames, err := readDifferentialT1KTestFrames(connection)
	if err != nil {
		t.Fatalf("read Oracle T1K response: %v; process=%s", err, processOutput.String())
	}
	if len(frames) != 5 || frames[0].tag != 0x41 || frames[4].tag != 0xa4 {
		t.Fatalf("Oracle T1K response frames = %#v", frames)
	}

	record := recordBase + ".0"
	for {
		if _, err := os.Stat(record); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Oracle T1K fixture did not record request: %s", processOutput.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read Oracle T1K fixture record: %v", err)
	}
	captured, err := parseDifferentialOracleFixtureRequest(raw)
	if err != nil {
		t.Fatalf("parse Oracle T1K fixture record: %v", err)
	}
	if captured.Method != http.MethodPost || captured.Path != "/oracle?wire=t1k" ||
		captured.Host != "gateway.example.test" || captured.Body != "body" {
		t.Fatalf("Oracle T1K embedded HTTP capture = %#v", captured)
	}
}

func TestDifferentialFixtureRejectsUnknownWireProtocol(t *testing.T) {
	_, err := startDifferentialFixture(DifferentialFixture{
		Name:         "invalid",
		WireProtocol: "auto",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported differential fixture wire protocol") {
		t.Fatalf("startDifferentialFixture() error = %v", err)
	}
}

type differentialT1KTestFrame struct {
	tag     byte
	payload string
}

func writeDifferentialT1KTestFrame(writer io.Writer, frame differentialT1KTestFrame) error {
	header := []byte{frame.tag, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(header[1:], uint32(len(frame.payload)))
	if _, err := writer.Write(header); err != nil {
		return err
	}
	_, err := io.WriteString(writer, frame.payload)
	return err
}

func differentialT1KTestBytes(frames ...differentialT1KTestFrame) []byte {
	var raw bytes.Buffer
	for _, frame := range frames {
		_ = writeDifferentialT1KTestFrame(&raw, frame)
	}
	return raw.Bytes()
}

func readDifferentialT1KTestFrames(reader io.Reader) ([]differentialT1KTestFrame, error) {
	var frames []differentialT1KTestFrame
	for {
		var header [5]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			return nil, err
		}
		payload := make([]byte, binary.LittleEndian.Uint32(header[1:]))
		if _, err := io.ReadFull(reader, payload); err != nil {
			return nil, err
		}
		frames = append(frames, differentialT1KTestFrame{tag: header[0], payload: string(payload)})
		if header[0]&0x80 != 0 {
			return frames, nil
		}
	}
}

func TestDifferentialSelectionPreflight(t *testing.T) {
	if os.Getenv("APISIX_GO_DIFFERENTIAL_PREFLIGHT") != "1" {
		t.Skip("set APISIX_GO_DIFFERENTIAL_PREFLIGHT=1 to validate differential selection")
	}
	if err := runDifferentialSelectionPreflight(os.LookupEnv, differentialCases(), os.Stdout); err != nil {
		t.Fatal(err)
	}
}

func TestRunDifferentialSelectionPreflightWritesNormalizedJSON(t *testing.T) {
	all := []DifferentialCase{
		{Name: "case-b", Plugin: "plugin-b"},
		{Name: "case-a", Plugin: "plugin-a"},
	}
	environment := map[string]string{
		differentialPluginsEnv:    " plugin-a ",
		differentialShardIndexEnv: "0",
		differentialShardCountEnv: "1",
	}
	getenv := func(name string) (string, bool) {
		value, ok := environment[name]
		return value, ok
	}
	var output bytes.Buffer
	if err := runDifferentialSelectionPreflight(getenv, all, &output); err != nil {
		t.Fatal(err)
	}
	want := "DIFFERENTIAL_SELECTION_JSON={\"plugins\":[\"plugin-a\"],\"cases\":[],\"shard_index\":0,\"shard_count\":1,\"selected_case_count\":1,\"full_catalog_run\":false}\n"
	if got := output.String(); got != want {
		t.Fatalf("preflight output = %q, want %q", got, want)
	}
}

func TestParseDifferentialSelectionFromEnvironment(t *testing.T) {
	all := []DifferentialCase{
		{Name: "case-a", Plugin: "plugin-a"},
		{Name: "case-b", Plugin: "plugin-b"},
		{Name: "case-c", Plugin: "plugin-a"},
	}
	environment := map[string]string{
		differentialPluginsEnv:    " plugin-a ",
		differentialCasesEnv:      "case-c, case-a",
		differentialShardIndexEnv: "0",
		differentialShardCountEnv: "2",
	}
	getenv := func(name string) (string, bool) {
		value, ok := environment[name]
		return value, ok
	}

	selected, normalized, err := parseDifferentialSelectionFromEnvironment(getenv, all)
	if err != nil {
		t.Fatalf("parseDifferentialSelectionFromEnvironment() error = %v", err)
	}
	if got, want := normalized.Plugins, []string{"plugin-a"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("normalized plugins = %v, want %v", got, want)
	}
	if got, want := normalized.Cases, []string{"case-a", "case-c"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("normalized cases = %v, want %v", got, want)
	}
	if normalized.ShardIndex != 0 || normalized.ShardCount != 2 {
		t.Fatalf("normalized shard = %d/%d, want 0/2", normalized.ShardIndex, normalized.ShardCount)
	}
	if len(selected) != 1 || selected[0].Name != "case-a" {
		t.Fatalf("selected cases = %#v, want case-a only", selected)
	}
}

func TestParseDifferentialSelectionFromEnvironmentRejectsInvalidValues(t *testing.T) {
	all := []DifferentialCase{{Name: "case-a", Plugin: "plugin-a"}}
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "blank plugin token", env: map[string]string{differentialPluginsEnv: "plugin-a,"}},
		{name: "present but empty plugin selector", env: map[string]string{differentialPluginsEnv: ""}},
		{name: "present but empty case selector", env: map[string]string{differentialCasesEnv: ""}},
		{name: "duplicate case", env: map[string]string{differentialCasesEnv: "case-a,case-a"}},
		{name: "unknown plugin", env: map[string]string{differentialPluginsEnv: "missing"}},
		{name: "invalid shard index", env: map[string]string{differentialShardIndexEnv: "bad"}},
		{name: "invalid shard count", env: map[string]string{differentialShardCountEnv: "0"}},
		{name: "empty shard index", env: map[string]string{differentialShardIndexEnv: ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(name string) (string, bool) {
				value, ok := tt.env[name]
				return value, ok
			}
			if _, _, err := parseDifferentialSelectionFromEnvironment(getenv, all); err == nil {
				t.Fatal("parseDifferentialSelectionFromEnvironment() error = nil")
			}
		})
	}
}

func TestRunDifferentialCaseBatchCapsWorkersAndPreservesOrder(t *testing.T) {
	cases := []DifferentialCase{
		{Name: "case-f", Plugin: "plugin-f"},
		{Name: "case-a", Plugin: "plugin-a"},
		{Name: "case-e", Plugin: "plugin-e"},
		{Name: "case-b", Plugin: "plugin-b"},
		{Name: "case-d", Plugin: "plugin-d"},
		{Name: "case-c", Plugin: "plugin-c"},
	}
	var active, maxActive int32
	var entered atomic.Int32
	start := make(chan struct{})
	var startOnce sync.Once
	results := runDifferentialCaseBatch(cases, func(spec DifferentialCase) DifferentialCaseResult {
		current := atomic.AddInt32(&active, 1)
		for {
			previous := atomic.LoadInt32(&maxActive)
			if current <= previous || atomic.CompareAndSwapInt32(&maxActive, previous, current) {
				break
			}
		}
		if entered.Add(1) == differentialWorkerLimit {
			startOnce.Do(func() { close(start) })
		}
		select {
		case <-start:
		case <-time.After(2 * time.Second):
			t.Fatalf("worker pool did not reach %d active workers", differentialWorkerLimit)
		}
		time.Sleep(5 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return DifferentialCaseResult{Name: spec.Name, Plugin: spec.Plugin, FirstAttempt: true, Passed: true}
	})

	if got, want := len(results), len(cases); got != want {
		t.Fatalf("result count = %d, want %d", got, want)
	}
	if got := atomic.LoadInt32(&maxActive); got != differentialWorkerLimit {
		t.Fatalf("maximum active workers = %d, want %d", got, differentialWorkerLimit)
	}
	if got := atomic.LoadInt32(&active); got != 0 {
		t.Fatalf("active workers after barrier = %d, want 0", got)
	}
	for index := 1; index < len(results); index++ {
		if results[index-1].Name >= results[index].Name {
			t.Fatalf("results are not canonical at %d: %#v", index, results)
		}
	}
}

func TestRunDifferentialCaseBatchContinuesAfterFailureAndPanic(t *testing.T) {
	cases := []DifferentialCase{
		{Name: "case-pass-a", Plugin: "plugin-a"},
		{Name: "case-fail", Plugin: "plugin-b"},
		{Name: "case-panic", Plugin: "plugin-c"},
		{Name: "case-pass-d", Plugin: "plugin-d"},
	}
	results := runDifferentialCaseBatch(cases, func(spec DifferentialCase) DifferentialCaseResult {
		switch spec.Name {
		case "case-fail":
			return DifferentialCaseResult{
				Name:         spec.Name,
				Plugin:       spec.Plugin,
				FirstAttempt: true,
				Error:        "deliberate failure",
			}
		case "case-panic":
			panic("deliberate panic")
		default:
			return DifferentialCaseResult{Name: spec.Name, Plugin: spec.Plugin, FirstAttempt: true, Passed: true}
		}
	})

	if len(results) != len(cases) {
		t.Fatalf("result count = %d, want %d", len(results), len(cases))
	}
	for _, result := range results {
		switch result.Name {
		case "case-fail":
			if result.Passed || result.Error == "" {
				t.Fatalf("failure result = %#v", result)
			}
		case "case-panic":
			if result.Passed || result.Error == "" {
				t.Fatalf("panic result = %#v", result)
			}
		default:
			if !result.Passed {
				t.Fatalf("peer result unexpectedly failed: %#v", result)
			}
		}
	}
}

func TestDifferentialResourceNamesAreBoundedAndDistinct(t *testing.T) {
	root := t.TempDir()
	first := DifferentialCase{Name: "consumer-restriction-basic-whitelist-jack1-allowed", Plugin: "plugin-a"}
	second := DifferentialCase{Name: "consumer-restriction-basic-whitelist-jack2-denied", Plugin: "plugin-a"}
	firstDir := differentialCaseWorkDir(root, 0, first)
	secondDir := differentialCaseWorkDir(root, 1, second)
	if firstDir == secondDir {
		t.Fatalf("case work directories collided: %q", firstDir)
	}
	const productionNonce = "0123456789abcdef0123456789abcdef"
	firstName := differentialOracleContainerName(productionNonce, first.Name, firstDir)
	secondName := differentialOracleContainerName(productionNonce, second.Name, secondDir)
	if firstName == secondName {
		t.Fatalf("oracle container names collided: %q", firstName)
	}
	if len(firstName) > differentialContainerNameLimit || len(secondName) > differentialContainerNameLimit {
		t.Fatalf("container names exceed limit: %q / %q", firstName, secondName)
	}
}

func TestDifferentialRequiredPluginNamesIncludeAttachConsumerLabelCollaborator(t *testing.T) {
	cases := differentialCasesForPlugin("attach-consumer-label")
	got := differentialRequiredPluginNames(cases)
	want := []string{"attach-consumer-label", "key-auth"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("differentialRequiredPluginNames() = %v, want %v", got, want)
	}
}

func TestDifferentialRequiredPluginNamesIncludeConsumerRestrictionCollaborator(t *testing.T) {
	cases := differentialCasesForPlugin("consumer-restriction")[1:2]
	got := differentialRequiredPluginNames(cases)
	want := []string{"basic-auth", "consumer-restriction"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("differentialRequiredPluginNames() = %v, want %v", got, want)
	}
}

func TestDifferentialRequiredPluginNamesIncludeACLCollaborator(t *testing.T) {
	got := differentialRequiredPluginNames(differentialCasesForPlugin("acl"))
	want := []string{"acl", "basic-auth"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("differentialRequiredPluginNames() = %v, want %v", got, want)
	}
}

func TestDifferentialRequiredPluginNamesIncludeMultiAuthChildren(t *testing.T) {
	got := differentialRequiredPluginNames(differentialCasesForPlugin("multi-auth"))
	want := []string{"basic-auth", "key-auth", "multi-auth"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("differentialRequiredPluginNames() = %v, want %v", got, want)
	}
}

func TestDifferentialRequiredPluginNamesAddPrometheusForAPISIXBatchProcessor(t *testing.T) {
	got := differentialRequiredPluginNames([]DifferentialCase{{Plugin: "http-logger"}})
	want := []string{"http-logger", "prometheus"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("differentialRequiredPluginNames() = %v, want %v", got, want)
	}
}

func TestDifferentialRequiredPluginNamesAddPrometheusForZipkinReporter(t *testing.T) {
	got := differentialRequiredPluginNames([]DifferentialCase{{Plugin: "zipkin"}})
	want := []string{"prometheus", "zipkin"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("differentialRequiredPluginNames() = %v, want %v", got, want)
	}
}

func TestDifferentialRequiredPluginNamesAddPrometheusForErrorLogLogger(t *testing.T) {
	got := differentialRequiredPluginNames([]DifferentialCase{{Plugin: "error-log-logger"}})
	want := []string{"error-log-logger", "prometheus"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("differentialRequiredPluginNames() = %v, want %v", got, want)
	}
}

func TestDifferentialErrorLogLoggerCaseLoadsOnlyItsPluginClosure(t *testing.T) {
	got := differentialRequiredPluginNames(differentialCasesForPlugin("error-log-logger"))
	want := []string{"basic-auth", "error-log-logger", "prometheus"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("error-log-logger plugin closure = %v, want %v", got, want)
	}
}

func TestStartDifferentialOracleReturnsCleanupOwnerAfterUncertainStart(t *testing.T) {
	root := t.TempDir()
	recordPath := filepath.Join(root, "runtime-calls")
	runtimePath := filepath.Join(root, "fake-container")
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$FAKE_RUNTIME_RECORD\"\nif [ \"$1\" = run ]; then exit 17; fi\nexit 0\n"
	if err := os.WriteFile(runtimePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_RUNTIME_RECORD", recordPath)

	child, err := startDifferentialOracle(
		runtimePath,
		OracleIdentity{ImageRepository: "example.test/apisix", ImageLinuxAMD64: "sha256:deadbeef"},
		"uncertain-oracle",
		filepath.Join(root, "config.yaml"),
		filepath.Join(root, "apisix.yaml"),
		nil,
	)
	if err == nil {
		t.Fatal("startDifferentialOracle() error = nil")
	}
	if child == nil {
		t.Fatal("startDifferentialOracle() child = nil, want cleanup owner")
	}
	if err := child.stop(); err != nil {
		t.Fatalf("cleanup uncertain start: %v", err)
	}
	calls, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(calls)); fmt.Sprint(got) != fmt.Sprint([]string{"run", "rm"}) {
		t.Fatalf("runtime calls = %v, want [run rm]", got)
	}
}

func TestDifferentialChildStopAcceptsVerifiedAbsenceAfterRemoveError(t *testing.T) {
	root := t.TempDir()
	recordPath := filepath.Join(root, "runtime-calls")
	runtimePath := filepath.Join(root, "fake-container")
	script := `#!/bin/sh
printf '%s %s\n' "$1" "${2:-}" >> "$FAKE_RUNTIME_RECORD"
if [ "$1" = rm ]; then exit 17; fi
if [ "$1" = container ] && [ "$2" = exists ]; then exit 1; fi
exit 99
`
	if err := os.WriteFile(runtimePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_RUNTIME_RECORD", recordPath)
	child := &differentialChild{container: true, runtime: runtimePath, name: "removed-oracle"}

	if err := child.stop(); err != nil {
		t.Fatalf("stop verified-absent oracle: %v", err)
	}
	calls, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(calls)); fmt.Sprint(got) !=
		fmt.Sprint([]string{"rm", "--force", "container", "exists"}) {
		t.Fatalf("runtime calls = %v, want rm followed by container exists", got)
	}
}

func TestDifferentialChildStopWaitsForRemovalToBecomeObservable(t *testing.T) {
	root := t.TempDir()
	recordPath := filepath.Join(root, "runtime-calls")
	statePath := filepath.Join(root, "runtime-state")
	runtimePath := filepath.Join(root, "fake-container")
	script := `#!/bin/sh
printf '%s %s\n' "$1" "${2:-}" >> "$FAKE_RUNTIME_RECORD"
if [ "$1" = rm ]; then exit 17; fi
if [ "$1" = container ] && [ "$2" = exists ]; then
    if [ ! -e "$FAKE_RUNTIME_STATE" ]; then
        : > "$FAKE_RUNTIME_STATE"
        exit 0
    fi
    exit 1
fi
exit 99
`
	if err := os.WriteFile(runtimePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_RUNTIME_RECORD", recordPath)
	t.Setenv("FAKE_RUNTIME_STATE", statePath)
	child := &differentialChild{container: true, runtime: runtimePath, name: "removing-oracle"}

	if err := child.stop(); err != nil {
		t.Fatalf("stop eventually-absent oracle: %v", err)
	}
	calls, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(calls)); fmt.Sprint(got) !=
		fmt.Sprint([]string{"rm", "--force", "container", "exists", "container", "exists"}) {
		t.Fatalf("runtime calls = %v, want rm followed by two absence checks", got)
	}
}

func TestWaitDifferentialCandidateDoneAfterKillWaitsForLogClosure(t *testing.T) {
	done := make(chan error, 1)
	closed := make(chan struct{})
	go func() {
		time.Sleep(25 * time.Millisecond)
		close(closed)
		done <- nil
	}()

	if err := waitDifferentialCandidateDoneAfterKill(done, time.Second); err != nil {
		t.Fatalf("waitDifferentialCandidateDoneAfterKill() error = %v", err)
	}
	select {
	case <-closed:
	default:
		t.Fatal("wait returned before candidate log closure")
	}
}

func TestFinalizeDifferentialOracleLateMismatchCapturesLogsBeforeRemoval(t *testing.T) {
	root := t.TempDir()
	recordPath := filepath.Join(root, "runtime-calls")
	runtimePath := filepath.Join(root, "fake-container")
	logPath := filepath.Join(root, "oracle.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$FAKE_RUNTIME_RECORD\"\nif [ \"$1\" = logs ]; then printf 'oracle late mismatch detail\\n'; fi\nexit 0\n"
	if err := os.WriteFile(runtimePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeDifferentialDiagnosticLog(logPath, "oracle was not started"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_RUNTIME_RECORD", recordPath)
	child := &differentialChild{
		done: make(chan error, 1), container: true, runtime: runtimePath, name: "late-mismatch",
	}
	result := DifferentialCaseResult{Passed: false, Error: "deliberate late mismatch"}

	finalizeDifferentialOracle(&result, child, logPath)

	calls, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(calls)); fmt.Sprint(got) != fmt.Sprint([]string{"logs", "rm"}) {
		t.Fatalf("runtime calls = %v, want logs before rm", got)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(
		logData,
	); !strings.Contains(got, "oracle late mismatch detail") ||
		strings.Contains(got, "was not started") {
		t.Fatalf("oracle log = %q, want real late-mismatch logs", got)
	}
}

func TestFinalizeDifferentialOracleReplacesPlaceholderWhenLogsUnavailable(t *testing.T) {
	root := t.TempDir()
	runtimePath := filepath.Join(root, "fake-container")
	logPath := filepath.Join(root, "oracle.log")
	script := "#!/bin/sh\nif [ \"$1\" = logs ]; then printf 'missing container' >&2; exit 1; fi\nexit 0\n"
	if err := os.WriteFile(runtimePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeDifferentialDiagnosticLog(logPath, "oracle was not started"); err != nil {
		t.Fatal(err)
	}
	child := &differentialChild{
		done: make(chan error, 1), container: true, runtime: runtimePath, name: "late-mismatch",
	}
	result := DifferentialCaseResult{Passed: false, Error: "deliberate late mismatch"}

	finalizeDifferentialOracle(&result, child, logPath)

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(
		logData,
	); !strings.Contains(got, "oracle logs unavailable") ||
		strings.Contains(got, "oracle was not started") {
		t.Fatalf("oracle log = %q, want explicit unavailable diagnostic", got)
	}
}
