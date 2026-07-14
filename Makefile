BINARY_NAME=apisix
GOLANGCI_LINT_VERSION ?= v2.12.2

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
