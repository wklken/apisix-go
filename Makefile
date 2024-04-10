BINARY_NAME=apisix

.PHONY: init
init:
	# for gofumpt
	go install mvdan.cc/gofumpt@latest
	# for golines
	go install github.com/segmentio/golines@latest


.PHONY: dep
dep:
	go mod tidy
	go mod vendor

.PHONY: fmt
fmt:
	golines ./ -m 120 -w --base-formatter gofmt --no-reformat-tags
	gofumpt -l -w .

.PHONY: build
build:
	go build -o ${BINARY_NAME}

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
