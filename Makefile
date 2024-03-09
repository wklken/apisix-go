BINARY_NAME=apisix

.PHONY: dep
dep:
	go mod tidy
	go mod vendor

.PHONY: build
build:
	go build -o ${BINARY_NAME}

.PHONY: serve
serve: build
	./apisix -c config.yaml

.PHONY: live
live:
	go run github.com/cosmtrek/air@v1.51.0 \
        --build.cmd "make build" --build.bin "./${BINARY_NAME} -c config.yaml" --build.delay "100" \
        --build.exclude_dir "" \
        --build.include_ext "go, tpl, tmpl, html, css, scss, js, ts, sql, jpeg, jpg, gif, png, bmp, svg, webp, ico" \
        --misc.clean_on_exit "true"