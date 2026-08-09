BINARY_NAME ?= apisix
BINARY_PATH ?= ./$(BINARY_NAME)
CACHE_BIN ?= $(if $(GOBIN),$(GOBIN),.cache/bin)
GOLANGCI_LINT_VERSION ?= v2.12.2

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_TIME ?= $(shell date +%Y-%m-%d_%H:%M:%S)
GO_VERSION ?= $(shell go version)

BENCHSTAT_VERSION ?= v0.0.0-20260709024250-82a0b07e230d
BENCH_DIR ?= .cache/bench
BENCHSTAT ?= $(CACHE_BIN)/benchstat
BENCH_PACKAGES ?= ./pkg/json ./pkg/plugin/base ./pkg/proxy ./pkg/route
BENCH_CORPUS_FILES ?= pkg/json/benchmark_test.go \
	pkg/plugin/base/logging_benchmark_test.go \
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
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}


.PHONY: dep
dep:
	go mod tidy
	go mod vendor

.PHONY: lint
lint:
	golangci-lint run

.PHONY: build
build:
	mkdir -p $(dir $(BINARY_PATH))
	go build -ldflags "-X github.com/wklken/apisix-go/pkg/version.Version=$(VERSION) -X github.com/wklken/apisix-go/pkg/version.Commit=$(COMMIT) -X github.com/wklken/apisix-go/pkg/version.BuildTime=$(BUILD_TIME) -X 'github.com/wklken/apisix-go/pkg/version.GoVersion=$(GO_VERSION)'" -o $(BINARY_PATH)

.PHONY: docker-build
docker-build:
	docker build --build-arg VERSION="$(VERSION)" --build-arg COMMIT="$(COMMIT)" --build-arg BUILD_TIME="$(BUILD_TIME)" --build-arg GO_VERSION="$(GO_VERSION)" -t apisix-go .

.PHONY: test
test:
	go test ./cmd/... ./pkg/... -count=1

COVERAGE_MIN ?= 82.0
COVERAGE_FILE ?= coverage.out

.PHONY: test-cover
test-cover:
	bash scripts/check-unit-coverage_test.sh
	COVERAGE_MIN=$(COVERAGE_MIN) ./scripts/check-unit-coverage.sh $(COVERAGE_FILE)

.PHONY: test-integration
test-integration:
	go test ./t/plugin -count=1 -v

.PHONY: serve
serve: build
	$(BINARY_PATH)

.PHONY: live
live:
	go run github.com/cosmtrek/air@v1.51.0 \
		--build.cmd "make build" --build.bin "$(BINARY_PATH)" --build.delay "100" \
		--build.exclude_dir "" \
		--build.include_ext "go, tpl, tmpl, html, css, scss, js, ts, sql, jpeg, jpg, gif, png, bmp, svg, webp, ico" \
		--misc.clean_on_exit "true"

.PHONY: cache-layout-test
cache-layout-test:
	bash scripts/cache_layout_test.sh

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
	GOBIN="$(CACHE_BIN)" go install golang.org/x/perf/cmd/benchstat@$(BENCHSTAT_VERSION)
	go version -m "$(BENCHSTAT)"

.PHONY: benchmark-runner-test
benchmark-runner-test:
	bash scripts/benchmark_test.sh

.PHONY: benchmark-smoke
benchmark-smoke:
	BENCH_TIME=100ms BENCH_COUNT=1 BENCH_CPU=1 bash scripts/benchmark.sh run smoke

.PHONY: benchmark-baseline
benchmark-baseline:
	bash scripts/benchmark.sh run baseline

.PHONY: benchmark-current
benchmark-current:
	bash scripts/benchmark.sh run current

.PHONY: benchmark
benchmark: benchmark-current

.PHONY: benchmark-compare
benchmark-compare:
	bash scripts/benchmark.sh compare baseline current

.PHONY: benchmark-profile-cpu
benchmark-profile-cpu:
	bash scripts/benchmark.sh profile-cpu $(PROFILE_PACKAGE) $(PROFILE_BENCH)

.PHONY: benchmark-profile-mem
benchmark-profile-mem:
	bash scripts/benchmark.sh profile-mem $(PROFILE_PACKAGE) $(PROFILE_BENCH)
