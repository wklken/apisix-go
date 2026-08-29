BINARY_NAME ?= apisix
BINARY_PATH ?= ./$(BINARY_NAME)
CACHE_BIN ?= $(if $(GOBIN),$(GOBIN),.cache/bin)
GOLANGCI_LINT_VERSION ?= v2.12.2
GO_CACHE_RUNNER ?= bash scripts/go_cache.sh run --
AIR_VERSION ?= v1.51.0
AIR ?= $(abspath $(CACHE_BIN))/air

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_TIME ?= $(shell date +%Y-%m-%d_%H:%M:%S)
GO_VERSION ?= $(shell go version)

BENCHSTAT_VERSION ?= v0.0.0-20260709024250-82a0b07e230d
BENCH_DIR ?= .cache/bench
BENCHSTAT ?= $(CACHE_BIN)/benchstat
BENCH_PACKAGES ?= ./pkg/json ./pkg/plugin ./pkg/plugin/base ./pkg/plugin/prometheus ./pkg/plugin/request_context ./pkg/proxy ./pkg/route
BENCH_CORPUS_FILES ?= pkg/json/benchmark_test.go \
	pkg/plugin/executor_benchmark_test.go \
	pkg/plugin/base/logging_benchmark_test.go \
	pkg/plugin/log_executor_benchmark_test.go \
	pkg/plugin/prometheus/benchmark_test.go \
	pkg/plugin/request_context/benchmark_test.go \
	pkg/proxy/benchmark_test.go \
	pkg/proxy/runtime_benchmark_test.go \
	pkg/route/benchmark_test.go \
	pkg/route/proxy_e2e_benchmark_test.go
BENCH_REGEX ?= .
BENCH_TIME ?= 1s
BENCH_COUNT ?= 10
BENCH_CPU ?= 1,4
BENCH_P ?= 1
BENCHMARK_VERSION ?= 1
# PROFILE_PACKAGE and PROFILE_BENCH are passed to the runner through make.
# Escape any $ in the regex as $$, e.g.
#   make benchmark-profile-cpu PROFILE_PACKAGE=./pkg/json PROFILE_BENCH='^BenchmarkFoo/size=1KiB$$'
PROFILE_PACKAGE ?=
PROFILE_BENCH ?=

export BENCH_DIR BENCH_PACKAGES BENCH_CORPUS_FILES BENCH_REGEX BENCH_TIME BENCH_COUNT BENCH_CPU BENCH_P BENCHMARK_VERSION BENCHSTAT BENCHSTAT_VERSION

.PHONY: init
init:
	$(GO_CACHE_RUNNER) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}


.PHONY: dep
dep:
	$(GO_CACHE_RUNNER) go mod tidy
	$(GO_CACHE_RUNNER) go mod vendor

.PHONY: lint
lint:
	$(GO_CACHE_RUNNER) golangci-lint run

.PHONY: build
build:
	mkdir -p $(dir $(BINARY_PATH))
	$(GO_CACHE_RUNNER) go build -trimpath -ldflags "-s -w -X github.com/wklken/apisix-go/pkg/version.Version=$(VERSION) -X github.com/wklken/apisix-go/pkg/version.Commit=$(COMMIT) -X github.com/wklken/apisix-go/pkg/version.BuildTime=$(BUILD_TIME) -X 'github.com/wklken/apisix-go/pkg/version.GoVersion=$(GO_VERSION)'" -o $(BINARY_PATH)

.PHONY: docker-build
docker-build:
	docker build --build-arg VERSION="$(VERSION)" --build-arg COMMIT="$(COMMIT)" --build-arg BUILD_TIME="$(BUILD_TIME)" --build-arg GO_VERSION="$(GO_VERSION)" -t apisix-go .

.PHONY: release-etcd-recovery
release-etcd-recovery:
	@test -n "$(APISIX_IMAGE)" || (printf 'APISIX_IMAGE is required\n' >&2; exit 1)
	bash scripts/etcd_recovery_smoke.sh "$(APISIX_IMAGE)"

.PHONY: test
test:
	$(GO_CACHE_RUNNER) go test ./cmd/... ./pkg/... -count=1

COVERAGE_MIN ?= 82.0
COVERAGE_FILE ?= coverage.out

.PHONY: test-cover
test-cover:
	$(GO_CACHE_RUNNER) bash scripts/check-unit-coverage_test.sh
	COVERAGE_MIN=$(COVERAGE_MIN) $(GO_CACHE_RUNNER) ./scripts/check-unit-coverage.sh $(COVERAGE_FILE)

.PHONY: test-integration
test-integration:
	$(GO_CACHE_RUNNER) go test ./t/plugin -count=1 -v

.PHONY: test-plugin-harness
test-plugin-harness:
	$(MAKE) test-rocketmq-client-patch
	APISIX_GO_SKIP_PLUGIN_INTEGRATION=1 $(GO_CACHE_RUNNER) go test ./t/plugin -count=1

.PHONY: test-rocketmq-patch
test-rocketmq-patch:
	$(GO_CACHE_RUNNER) bash scripts/qualification/rocketmq_patch_gate.sh

.PHONY: test-rocketmq-client-patch
test-rocketmq-client-patch: test-rocketmq-patch test-rocketmq-nested

.PHONY: test-rocketmq-nested
test-rocketmq-nested:
	cd third_party/rocketmq-client-go && ../../scripts/go_cache.sh run -- go test ./internal/remote -run '^(TestTLSHandshakeHonorsContext|TestTLSVerificationUsesRootsAndAddressServerName|TestTLSVerificationRejectsUnknownAuthority|TestTLSVerificationRejectsWrongServerName|TestTLSCompatibilityModeSkipsVerification|TestInvokeOneWayHonorsContextWhileConnectionLockIsHeld|TestDoRequestHonorsContextWhileConnectionWriteLockIsHeld|TestDoRequestCancelsBlockedWrite|TestDoRequestWaitsForCancellationDeadlineCallbackBeforeReusingConnection|TestSendRequestPassesCallerContextThroughInterceptor)$$' -count=1
	cd third_party/rocketmq-client-go && ../../scripts/go_cache.sh run -- go test ./internal -run '^(TestTopicRouteLockHonorsContext|TestDefaultClientOptionsOwnsRemotingConfig|TestStartTaskCancellationSkipsDelayedOperation|TestRMQClientShutdownJoinsAllStartTasks)$$' -count=1
	cd third_party/rocketmq-client-go && ../../scripts/go_cache.sh run -- go test ./producer -run '^(TestSendSyncStopsBeforeRetryWhenContextIsCanceled|TestSendOneWayStopsBeforeRetryWhenContextIsCanceled|TestTLSOptionsPropagateToEachProducerRemotingConfig)$$' -count=1

.PHONY: generate-capabilities
generate-capabilities:
	$(GO_CACHE_RUNNER) go run ./cmd/capability-gen -repo-root . -write

.PHONY: check-capability-drift
check-capability-drift:
	$(GO_CACHE_RUNNER) go run ./cmd/capability-gen -repo-root . -check

.PHONY: test-capability-status
test-capability-status:
	$(GO_CACHE_RUNNER) go test ./pkg/capability ./pkg/config ./pkg/plugin -run '^(TestLoadedManifest|TestManifest|TestProfileSelection|TestCapabilityManifest|TestCapabilityRegistry)' -count=1
	APISIX_GO_SKIP_PLUGIN_INTEGRATION=1 $(GO_CACHE_RUNNER) go test ./t/plugin -run '^(TestCapabilityManifestSelection|TestManifestCorpusValidates|TestUpstreamCorpusAccountingWithoutSourceCheckout|TestCorpusEvidenceMatchesCompatibilityTarget)$$' -count=1

.PHONY: test-plugin-behavior-gate
test-plugin-behavior-gate:
	bash scripts/qualification/plugin_behavior_gate_test.sh

.PHONY: qualify-plugin-behavior
qualify-plugin-behavior:
	bash scripts/qualification/plugin_behavior_gate.sh

PLUGIN_SMOKE_CASE ?=

.PHONY: test-plugin-smoke
test-plugin-smoke:
	@test -n "$(strip $(PLUGIN_SMOKE_CASE))" || (printf 'PLUGIN_SMOKE_CASE is required\n' >&2; exit 1)
	APISIX_GO_PLUGIN_SMOKE_CASE="$(PLUGIN_SMOKE_CASE)" $(GO_CACHE_RUNNER) go test ./t/plugin -run '^TestPluginIntegration$$' -count=1 -v

.PHONY: serve
serve: build
	$(BINARY_PATH)

.PHONY: live
live:
	mkdir -p $(dir $(AIR))
	$(GO_CACHE_RUNNER) env GOBIN="$(abspath $(CACHE_BIN))" go install github.com/cosmtrek/air@$(AIR_VERSION)
	$(AIR) \
		--build.cmd "make build" --build.bin "$(BINARY_PATH)" --build.delay "100" \
		--build.exclude_dir "" \
		--build.include_ext "go, tpl, tmpl, html, css, scss, js, ts, sql, jpeg, jpg, gif, png, bmp, svg, webp, ico" \
		--misc.clean_on_exit "true"

.PHONY: cache-layout-test
cache-layout-test:
	bash scripts/cache_layout_test.sh

.PHONY: cache-gc-test
cache-gc-test:
	bash scripts/go_cache_test.sh

.PHONY: cache-status
cache-status:
	@test -n "$(APISIX_GO_SHARED_CACHE)" || \
		(printf 'source .envrc before running cache-status\n' >&2; exit 1)
	@printf 'shared cache: %s\n' "$(APISIX_GO_SHARED_CACHE)"
	@printf 'local cache:  %s/.cache\n' "$(CURDIR)"
	@printf 'GOMODCACHE:   %s\n' "$(GOMODCACHE)"
	@printf 'GOCACHE:      %s\n' "$(GOCACHE)"
	@printf 'GOBIN:        %s\n' "$(GOBIN)"
	@du -sh "$(APISIX_GO_SHARED_CACHE)" .cache 2>/dev/null || true
	@bash scripts/go_cache.sh status

.PHONY: cache-gc
cache-gc:
	bash scripts/go_cache.sh gc

.PHONY: cache-clean-shared
cache-clean-shared:
	bash scripts/go_cache.sh clean

.PHONY: clean
clean:
	rm -f "$(BINARY_PATH)" ./apisix

.PHONY: cache-clean-local
cache-clean-local: clean
	@if [ -d .cache/go-mod ]; then chmod -R u+w .cache/go-mod; fi
	rm -rf \
		.cache/tmp \
		.cache/telemetry \
		.cache/out \
		.cache/go \
		.cache/go-build \
		.cache/go-mod \
		.cache/bin \
		.cache/golangci-lint

.PHONY: init-bench
init-bench:
	GOBIN="$(CACHE_BIN)" $(GO_CACHE_RUNNER) go install golang.org/x/perf/cmd/benchstat@$(BENCHSTAT_VERSION)
	$(GO_CACHE_RUNNER) go version -m "$(BENCHSTAT)"

.PHONY: benchmark-runner-test
benchmark-runner-test:
	bash scripts/benchmark_test.sh

.PHONY: benchmark-smoke
benchmark-smoke:
	BENCH_TIME=100ms BENCH_COUNT=1 BENCH_CPU=1 $(GO_CACHE_RUNNER) bash scripts/benchmark.sh run smoke

.PHONY: benchmark-baseline
benchmark-baseline:
	$(GO_CACHE_RUNNER) bash scripts/benchmark.sh run baseline

.PHONY: benchmark-current
benchmark-current:
	$(GO_CACHE_RUNNER) bash scripts/benchmark.sh run current

.PHONY: benchmark
benchmark: benchmark-current

.PHONY: benchmark-compare
benchmark-compare:
	bash scripts/benchmark.sh compare baseline current

.PHONY: benchmark-profile-cpu
benchmark-profile-cpu:
	$(GO_CACHE_RUNNER) bash scripts/benchmark.sh profile-cpu $(PROFILE_PACKAGE) $(PROFILE_BENCH)

.PHONY: benchmark-profile-mem
benchmark-profile-mem:
	$(GO_CACHE_RUNNER) bash scripts/benchmark.sh profile-mem $(PROFILE_PACKAGE) $(PROFILE_BENCH)
