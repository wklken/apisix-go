FROM golang:1.26.5 AS builder

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

RUN go build -ldflags "-X github.com/wklken/apisix-go/pkg/version.Version=${VERSION} -X github.com/wklken/apisix-go/pkg/version.Commit=${COMMIT} -X github.com/wklken/apisix-go/pkg/version.BuildTime=${BUILD_TIME} -X 'github.com/wklken/apisix-go/pkg/version.GoVersion=${GO_VERSION}'" -o /apisix

# deploy
FROM alpine:3.19


RUN mkdir -p /usr/local/apisix/conf/
RUN mkdir -p /usr/local/apisix/logs/

WORKDIR /usr/local/apisix

COPY conf/config.yaml conf/config-default.yaml /usr/local/apisix/conf/

COPY --from=builder /apisix /usr/bin/apisix

ENTRYPOINT [ "/usr/bin/apisix", "-c", "/usr/local/apisix/conf/config.yaml" ]
