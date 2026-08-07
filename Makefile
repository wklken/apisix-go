BINARY_NAME=apisix
GOLANGCI_LINT_VERSION ?= v2.12.2

BENCHSTAT_VERSION ?= v0.0.0-20260709024250-82a0b07e230d
BENCH_DIR ?= .cache/bench
BENCHSTAT ?= .cache/bin/benchstat
BENCH_PACKAGES ?= ./pkg/json ./pkg/plugin/base ./pkg/proxy ./pkg/route
BENCH_CORPUS_FILES ?= pkg/json/benchmark_test.go \
	pkg/plugin/base/logging_benchmark_test.go \
	pkg/proxy/benchmark_test.go \
	pkg/route/benchmark_test.go
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
	go build -o ${BINARY_NAME}

.PHONY: test
test:
	go test ./cmd/... ./pkg/... -count=1

.PHONY: test-integration
test-integration:
	go test ./t/plugin -count=1 -v

.PHONY: serve
serve: build
	./apisix

.PHONY: live
live:
	go run github.com/cosmtrek/air@v1.51.0 \
        --build.cmd "make build" --build.bin "./${BINARY_NAME}" --build.delay "100" \
        --build.exclude_dir "" \
        --build.include_ext "go, tpl, tmpl, html, css, scss, js, ts, sql, jpeg, jpg, gif, png, bmp, svg, webp, ico" \
        --misc.clean_on_exit "true"

.PHONY: init-bench
init-bench:
	GOBIN=$(CURDIR)/.cache/bin go install golang.org/x/perf/cmd/benchstat@$(BENCHSTAT_VERSION)
	go version -m $(BENCHSTAT)

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
