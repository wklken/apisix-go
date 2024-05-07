FROM golang:1.22.2-alpine3.19 AS builder

# build
WORKDIR /app

COPY go.mod /app/
COPY go.sum /app/
RUN go mod download

COPY main.go /app/
COPY cmd /app/cmd
COPY pkg /app/pkg

RUN go build -o /apisix

# deploy
FROM alpine:3.19


RUN mkdir -p /usr/local/apisix/conf/
RUN mkdir -p /usr/local/apisix/logs/

WORKDIR /usr/local/apisix

COPY conf/config*.yaml /usr/local/apisix/conf/

COPY --from=builder /apisix /usr/bin/apisix

ENTRYPOINT [ "/usr/bin/apisix", "-c", "/usr/local/apisix/conf/config.yaml" ]
