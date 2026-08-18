FROM golang:1.26.6-alpine3.24 AS builder

# build
WORKDIR /app

ARG VERSION=0.1.0
ARG COMMIT=none
ARG BUILD_TIME=unknown
ARG GO_VERSION=unknown

COPY go.mod /app/
COPY go.sum /app/
RUN go mod download

COPY main.go /app/
COPY cmd /app/cmd
COPY pkg /app/pkg

RUN go build -trimpath -ldflags "-s -w -X github.com/wklken/apisix-go/pkg/version.Version=${VERSION} -X github.com/wklken/apisix-go/pkg/version.Commit=${COMMIT} -X github.com/wklken/apisix-go/pkg/version.BuildTime=${BUILD_TIME} -X 'github.com/wklken/apisix-go/pkg/version.GoVersion=${GO_VERSION}'" -o /apisix

# deploy
FROM alpine:3.24.1

RUN apk add --no-cache ca-certificates curl \
    && addgroup -S -g 10001 apisix \
    && adduser -S -D -H -u 10001 -G apisix apisix \
    && mkdir -p /usr/local/apisix/conf /usr/local/apisix/logs /usr/local/apisix/data \
    && chown -R apisix:apisix /usr/local/apisix

WORKDIR /usr/local/apisix

COPY --chown=apisix:apisix conf/config.yaml conf/config-default.yaml conf/config-production.yaml /usr/local/apisix/conf/

COPY --from=builder /apisix /usr/bin/apisix

USER 10001:10001

HEALTHCHECK --interval=5s --timeout=3s --start-period=10s --retries=12 \
    CMD curl --fail --silent --show-error --output /dev/null http://127.0.0.1:9080/livez || exit 1

ENTRYPOINT ["/usr/bin/apisix"]
CMD ["-c", "/usr/local/apisix/conf/config-production.yaml"]
