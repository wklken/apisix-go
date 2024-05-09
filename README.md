# apisix-go

This is an [apache/apisix](https://github.com/apache/apisix) Data Plane(DP) implemented via Go

NOT READY FOR PRODUCTION!

## Features

- Small binary size(<50M) and image size (<60M)
- Easy to deploy and scale
- Better performance with io plugins like `*-logger`
- Easy to extend with Go http middlewares or Go Plugins(develop and test is much easier)

### Supported

- [x] Route
- [x] Service
- [x] Upstream
- [x] Plugin Metadata
- [x] Global Rules
- [x] Plugin Attr
- [x] Consumer
- [x] Plugin Config
- [ ] Consumer Group
- [ ] Script
- [ ] Secret

### Not Supported

- http method `PURGE` is not supported

## Plugins

> still working on it

### General

> 3/7

- [ ] [batch-requests](https://apisix.apache.org/zh/docs/apisix/plugins/batch-requests/)
- [x] [redirect](https://apisix.apache.org/zh/docs/apisix/plugins/redirect/)
  - not support regex_uri
  - not support encode_uri
  - not support plugin_attr get random https port from apisix.ssl.listen
- [ ] [echo](https://apisix.apache.org/zh/docs/apisix/plugins/echo/)
- [x] [gzip](https://apisix.apache.org/zh/docs/apisix/plugins/gzip/) 90%
  - not support `types = ["*"]`
  - not support `min_length`
  - not support `buffers`(it's nginx native feature)
- [ ] [brotli](https://apisix.apache.org/zh/docs/apisix/plugins/brotli/)
- [x] [real-ip](https://apisix.apache.org/zh/docs/apisix/plugins/real-ip/) 100%
- [ ] [server-info](https://apisix.apache.org/zh/docs/apisix/plugins/server-info/)
- &#x2612; [ext-plugin-pre-req](https://apisix.apache.org/zh/docs/apisix/plugins/ext-plugin-pre-req/)      NOT SUPPORTED, No need
- &#x2612; [ext-plugin-post-req](https://apisix.apache.org/zh/docs/apisix/plugins/ext-plugin-post-req/)    NOT SUPPORTED, No need
- &#x2612; [ext-plugin-post-resp](https://apisix.apache.org/zh/docs/apisix/plugins/ext-plugin-post-resp/)  NOT SUPPORTED, No need
- &#x2612; [inspect](https://apisix.apache.org/zh/docs/apisix/plugins/inspect/)                            NOT SUPPORTED, lua feature
- &#x2612; [ocsp-stapling](https://apisix.apache.org/zh/docs/apisix/plugins/ocsp-stapling/)                NOT SUPPORTED, nginx feature

### Transformation

> 3/8

- [ ] [response-rewrite](https://apisix.apache.org/zh/docs/apisix/plugins/response-rewrite/)
- [x] [proxy-rewrite](https://apisix.apache.org/zh/docs/apisix/plugins/proxy-rewrite/) 80%
  - not support `regex_uri`
  - not support `use_real_request_uri_unsafe`
- [ ] [grpc-transcode](https://apisix.apache.org/zh/docs/apisix/plugins/grpc-transcode/)
- [ ] [grpc-web](https://apisix.apache.org/zh/docs/apisix/plugins/grpc-web/)
- [x] [fault-injection](https://apisix.apache.org/zh/docs/apisix/plugins/fault-injection/)
- [x] [mocking](https://apisix.apache.org/zh/docs/apisix/plugins/mocking/) 90%
  - not support `response_schema`
- [ ] [degraphql](https://apisix.apache.org/zh/docs/apisix/plugins/degraphql/)
- [ ] [body-transformer](https://apisix.apache.org/zh/docs/apisix/plugins/body-transformer/)

### Authentication

> 2/15

- [x] [key-auth](https://apisix.apache.org/zh/docs/apisix/plugins/key-auth/)
- [ ] [jwt-auth](https://apisix.apache.org/zh/docs/apisix/plugins/jwt-auth/)
- [ ] [jwe-decrypt](https://apisix.apache.org/zh/docs/apisix/plugins/jwe-decrypt/)
- [x] [basic-auth](https://apisix.apache.org/zh/docs/apisix/plugins/basic-auth/)
- [ ] [authz-keycloak](https://apisix.apache.org/zh/docs/apisix/plugins/authz-keycloak/)
- [ ] [authz-casdoor](https://apisix.apache.org/zh/docs/apisix/plugins/authz-casdoor/)
- [ ] [wolf-rbac](https://apisix.apache.org/zh/docs/apisix/plugins/wolf-rbac/)
- [ ] [openid-connect](https://apisix.apache.org/zh/docs/apisix/plugins/openid-connect/)
- [ ] [cas-auth](https://apisix.apache.org/zh/docs/apisix/plugins/cas-auth/)
- [ ] [hmac-auth](https://apisix.apache.org/zh/docs/apisix/plugins/hmac-auth/)
- [ ] [authz-casbin](https://apisix.apache.org/zh/docs/apisix/plugins/authz-casbin/)
- [ ] [ldap-auth](https://apisix.apache.org/zh/docs/apisix/plugins/ldap-auth/)
- [ ] [opa](https://apisix.apache.org/zh/docs/apisix/plugins/opa/)
- [ ] [forward-auth](https://apisix.apache.org/zh/docs/apisix/plugins/forward-auth/)
- [ ] [multi-auth](https://apisix.apache.org/zh/docs/apisix/plugins/multi-auth/)

### Security

> 7/10

- [x] [cors](https://apisix.apache.org/zh/docs/apisix/plugins/cors/)
- [x] [uri-blocker](https://apisix.apache.org/zh/docs/apisix/plugins/uri-blocker/) 100%
- [x] [ip-restriction](https://apisix.apache.org/zh/docs/apisix/plugins/ip-restriction/) 100%
- [x] [ua-restriction](https://apisix.apache.org/zh/docs/apisix/plugins/ua-restriction/) 100%
- [x] [referer-restriction](https://apisix.apache.org/zh/docs/apisix/plugins/referer-restriction/) 100%
- [x] [consumer-restriction](https://apisix.apache.org/zh/docs/apisix/plugins/consumer-restriction/)
- [x] [csrf](https://apisix.apache.org/zh/docs/apisix/plugins/csrf/)
- [ ] [public-api](https://apisix.apache.org/zh/docs/apisix/plugins/public-api/)
- [ ] [GM](https://apisix.apache.org/zh/docs/apisix/plugins/GM/)
- [ ] [chaitin-waf](https://apisix.apache.org/zh/docs/apisix/plugins/chaitin-waf/)

### Traffic

> 5/12

- [ ] [limit-req](https://apisix.apache.org/zh/docs/apisix/plugins/limit-req/)
- [ ] [limit-conn](https://apisix.apache.org/zh/docs/apisix/plugins/limit-conn/)
- [x] [limit-count](https://apisix.apache.org/zh/docs/apisix/plugins/limit-count/) 50%
  - keys todo
  - redis-cluster todo
- [ ] [proxy-cache](https://apisix.apache.org/zh/docs/apisix/plugins/proxy-cache/)
- [x] [request-validation](https://apisix.apache.org/zh/docs/apisix/plugins/request-validation/)
  - not support `header_schema`
- [ ] [proxy-mirror](https://apisix.apache.org/zh/docs/apisix/plugins/proxy-mirror/)
- [x] [api-breaker](https://apisix.apache.org/zh/docs/apisix/plugins/api-breaker/) 90%
  - not support `healthy.http_statuse`
  - not support `break_response_headers` vars
- [ ] [traffic-split](https://apisix.apache.org/zh/docs/apisix/plugins/traffic-split/)
- [x] [request-id](https://apisix.apache.org/zh/docs/apisix/plugins/request-id/) 100%
- [ ] [proxy-control](https://apisix.apache.org/zh/docs/apisix/plugins/proxy-control/)
- [x] [client-control](https://apisix.apache.org/zh/docs/apisix/plugins/client-control/) 100%
- [ ] [workflow](https://apisix.apache.org/zh/docs/apisix/plugins/workflow/)

### Observability

Tracers:

> 0/3

- [ ] [zipkin](https://apisix.apache.org/zh/docs/apisix/plugins/zipkin/)
- [ ] [skywalking](https://apisix.apache.org/zh/docs/apisix/plugins/skywalking/)
- [ ] [opentelemetry](https://apisix.apache.org/zh/docs/apisix/plugins/opentelemetry/)

Metrics:

> 1/3

- [x] [prometheus](https://apisix.apache.org/zh/docs/apisix/plugins/prometheus/)
- [ ] [node-status](https://apisix.apache.org/zh/docs/apisix/plugins/node-status/)
- [ ] [datadog](https://apisix.apache.org/zh/docs/apisix/plugins/datadog/)

Loggers:

> 6/18

- [x] [http-logger](https://apisix.apache.org/zh/docs/apisix/plugins/http-logger/)
  - not support `include_req_body` and `include_req_body_expr`
  - not support `include_resp_body` and `include_resp_body_expr`
- [ ] [skywalking-logger](https://apisix.apache.org/zh/docs/apisix/plugins/skywalking-logger/)
- [x] [tcp-logger](https://apisix.apache.org/zh/docs/apisix/plugins/tcp-logger/)
  - not support `tls` and `tls_options`
  - not support `include_req_body` and `include_req_body_expr`
  - not support `include_resp_body` and `include_resp_body_expr`
- [ ] [kafka-logger](https://apisix.apache.org/zh/docs/apisix/plugins/kafka-logger/)
- [ ] [rocketmq-logger](https://apisix.apache.org/zh/docs/apisix/plugins/rocketmq-logger/)
- [x] [udp-logger](https://apisix.apache.org/zh/docs/apisix/plugins/udp-logger/)
  - not support `include_req_body` and `include_req_body_expr`
  - not support `include_resp_body` and `include_resp_body_expr`
- [ ] [clickhouse-logger](https://apisix.apache.org/zh/docs/apisix/plugins/clickhouse-logger/)
- [x] [syslog](https://apisix.apache.org/zh/docs/apisix/plugins/syslog/)
- [ ] [log-rotate](https://apisix.apache.org/zh/docs/apisix/plugins/log-rotate/)
- [ ] [error-log-logger](https://apisix.apache.org/zh/docs/apisix/plugins/error-log-logger/)
- [ ] [sls-logger](https://apisix.apache.org/zh/docs/apisix/plugins/sls-logger/)
- [ ] [google-cloud-logging](https://apisix.apache.org/zh/docs/apisix/plugins/google-cloud-logging/)
- [ ] [splunk-hec-logging](https://apisix.apache.org/zh/docs/apisix/plugins/splunk-hec-logging/)
- [x] [file-logger](https://apisix.apache.org/zh/docs/apisix/plugins/file-logger/) 50%
  - not support `include_req_body` and `include_req_body_expr`
  - not support `include_resp_body` and `include_resp_body_expr`
  - not support `match`
- [ ] [loggly](https://apisix.apache.org/zh/docs/apisix/plugins/loggly/)
- [x] [elasticsearch-logger](https://apisix.apache.org/zh/docs/apisix/plugins/elasticsearch-logger/)
- [ ] [tencent-cloud-cls](https://apisix.apache.org/zh/docs/apisix/plugins/tencent-cloud-cls/)
- [ ] [loki-logger](https://apisix.apache.org/zh/docs/apisix/plugins/loki-logger/)

## TODO

- [ ] standalone mode
- [ ] handle etcd compact
- [ ] github action go releaser
- [ ] logforamt change didn't take effect immediately

- [ ] plugin limit-req [text](https://apisix.apache.org/zh/docs/apisix/plugins/limit-req/)
- [ ] plugin limit-conn [text](https://apisix.apache.org/zh/docs/apisix/plugins/limit-conn/) hard
- [ ] plugin proxy-rewrite [text](https://apisix.apache.org/zh/docs/apisix/plugins/proxy-rewrite/) 剩余功能
- [ ] plugin response-rewrite [text](https://apisix.apache.org/zh/docs/apisix/plugins/response-rewrite/) a little hard
- [ ] plugin server-info [text](https://apisix.apache.org/zh/docs/apisix/plugins/server-info/)
  - [ ] register self to `/apisix/data_plane/server_info/{server_id}`

- [ ] admin api
- [ ] consumer group id => consumer dynamic plugins
- [ ] how to impl the serverless
- [ ] 插件优先级 Consumer > Consumer Group > Route > Plugin Config > Service, 目前没有Consumer, 所以只需要再支持 Plugin Config
- [ ] plugin brotli [brotli](https://apisix.apache.org/zh/docs/apisix/plugins/brotli/) via [text](https://pkg.go.dev/github.com/andybalholm/brotli#section-readme)
- [ ] jwt [go-jose](https://github.com/go-jose/go-jose/)
- [ ] route + service + upstream, merge the config
- [ ] read the conf/config-default.yaml and conf/config.yaml, and merge the config
- [ ] the plugin which modify response, how?