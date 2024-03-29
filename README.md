# apisix-go

This is an `apisix` implemented via Go

just a toy project for now, not for production use.

no lint, no tests, no docs! I will use any libs, just for fun!

I will try to implement the `apisix` features one by one.

Build in public!

Let's see how far I can go.

## Supported Features

Plugins: (4/50)

- [x] proxy-rewrite 80%
  - not support `regex_uri`
  - not support `use_real_request_uri_unsafe`
- [x] mocking 90%
  - not support `response_schema`
- [x] client-control 100%
- [x] request-id 100%
- [x] uri-blocker 100%

## DONE


- [x] choose router => [chi](https://github.com/go-chi/chi) 2024-03-08
- [x] reverse proxy => [httputil/reverseproxy](https://go.dev/src/net/http/httputil/reverseproxy.go) 2024-03-08
- [x] bpool  => [bpool](http://github.com/oxtoacart/bpool)
- [x] etcd fetch all + watch => [etcd/client/v3](https://pkg.go.dev/go.etcd.io/etcd/client/v3) 2024-03-08
- [x] local kv storage  => [bbolt](https://github.com/etcd-io/bbolt) 2024-03-08
- [x] loadbalance weighted rr => [weighted](http://github.com/smallnest/weighted) 2024-03-08
- [x] plugin model 2024-03-09
- [x] plugin chain => [alice](https://github.com/justinas/alice) 2024-03-09
- [x] demo etcd config to httpbin get => httpbin.org 2024-03-09
- [x] chi graceful shutdown 2024-03-09
- [x] json lib => [go-json](https://github.com/goccy/go-json) 2024-03-09
- [x] plugin config validate => [jsonschema](https://github.com/santhosh-tekuri/jsonschema) 2024-03-09
- [x] add prometheus => [client_golang](https://github.com/prometheus/client_golang) 2024-03-10
- [x] base apisix context for all plugins 2024-03-10
- [x] add otel 2024-03-11
- [x] add config file and parse => [viper](https://github.com/spf13/viper) 2024-03-12
- [x] add redis client => [rueidis](https://github.com/redis/rueidis) 2024-03-13
- [x] add local memory cache(lrucache) => [golang-lru](https://github.com/hashicorp/golang-lru) 2024-03-14
- [x] rebuild the whole radixtree after the route/service/upstrem changed 2024-03-16
- [x] watch and use the real data from etcd  2024-03-17
- [x] add get pluginmetadata 2024-03-18
- [x] convert apisix uri to chi uri 2024-03-19
- [x] plugin: proxy-rewrite according to  [proxy-rewrite](https://apisix.apache.org/docs/apisix/plugins/proxy-rewrite/) 2024-03-20
- [x] use go-resty/rest  => [go-resty/rest](https://github.com/go-resty/resty) 2024-03-21
- [x] add plugin ctx utils => inspired by [gin/context.go](https://github.com/gin-gonic/gin/blob/7a865dcf1dbe6ec52e074b1ddce830d278eb72cf/context.go) 2024-03-24
- [x] plugin mocking => [apisix doc: mocking](https://apisix.apache.org/zh/docs/apisix/plugins/mocking/) 2024-03-26
- [x] plugin client-control [apisix doc: client-control](https://apisix.apache.org/zh/docs/apisix/plugins/client-control/) 2024-03-27
- [x] plugin request-id [apisix doc: request-id](https://apisix.apache.org/zh/docs/apisix/plugins/request-id/) 2024-03-28
- [ ] plugin uri-blocker [apisix doc: uri-blocker](https://apisix.apache.org/zh/docs/apisix/plugins/uri-blocker/) 2024-03-29

## doing

- [ ] global rules => 插件的优先级最高 [text](https://apisix.apache.org/zh/docs/apisix/terminology/global-rule/)
- [ ] plugin metadata => 如果没有自定义,会使用metadata中定义的 [text](https://apisix.apache.org/zh/docs/apisix/terminology/plugin-metadata/)

## TODO

- [ ] plugin ua-restriction [text](https://apisix.apache.org/zh/docs/apisix/plugins/ua-restriction/)
- [ ] plugin referer-restriction [text](https://apisix.apache.org/zh/docs/apisix/plugins/referer-restriction/)

- [ ] plugin request-validation [text](https://apisix.apache.org/zh/docs/apisix/plugins/request-validation/)

- [ ] plugin limit-req [text](https://apisix.apache.org/zh/docs/apisix/plugins/limit-req/)
- [ ] plugin limit-count [text](https://apisix.apache.org/zh/docs/apisix/plugins/limit-count/)
- [ ] plugin limit-conn [text](https://apisix.apache.org/zh/docs/apisix/plugins/limit-conn/) hard

- [ ] plugin cors [text](https://apisix.apache.org/zh/docs/apisix/plugins/cors/) easy
- [ ] plugin redirect [text](https://apisix.apache.org/zh/docs/apisix/plugins/redirect/)
- [ ] plugin gzip [text](https://apisix.apache.org/zh/docs/apisix/plugins/gzip/)

- [ ] plugin ip-restriction [text](https://apisix.apache.org/zh/docs/apisix/plugins/ip-restriction/)

- [ ] plugin api-breaker [text](https://apisix.apache.org/zh/docs/apisix/plugins/api-breaker/)

- [ ] plugin file-logger [text](https://apisix.apache.org/zh/docs/apisix/plugins/file-logger/) easy
- [ ] handle etcd compact
- [ ] 插件优先级 Consumer > Consumer Group > Route > Plugin Config > Service, 目前没有Consumer, 所以只需要再支持 Plugin Config
- [ ] plugin config id in route
- [ ] plugin real-ip [text](https://apisix.apache.org/zh/docs/apisix/plugins/real-ip/)
- [ ] plugin response-rewrite [text](https://apisix.apache.org/zh/docs/apisix/plugins/response-rewrite/) a little hard
- [ ] plugin proxy-rewrite [text](https://apisix.apache.org/zh/docs/apisix/plugins/proxy-rewrite/) 剩余功能
- [ ] plugin fault-injection [text](https://apisix.apache.org/zh/docs/apisix/plugins/fault-injection/)
- [ ] plugin key-auth [text](https://apisix.apache.org/zh/docs/apisix/plugins/key-auth/) ?
- [ ] plugin csrf [text](https://apisix.apache.org/zh/docs/apisix/plugins/csrf/)
- [ ] plugin http-logger [text](https://apisix.apache.org/zh/docs/apisix/plugins/http-logger/)
- [ ] plugin sys-logger [text](https://apisix.apache.org/zh/docs/apisix/plugins/syslog/)
- [ ] plugin tcp-logger [text](https://apisix.apache.org/zh/docs/apisix/plugins/tcp-logger/)
- [ ] plugin udp-logger [text](https://apisix.apache.org/zh/docs/apisix/plugins/udp-logger/)

- [ ] how to impl the serverless

- [ ] admin api
- [ ] register self to `/apisix/data_plane/server_info/{server_id}`
- [ ] plugins
  - cors
  - basic_auth
  - file-logger
  - syslog
  - http-logger
  - ip-restriction
  - limit-count
  - prometheus
  - opentelemetry
- [ ] plugins
  - tcp-logger
  - udp-logger
- [ ] jwt [go-jose](https://github.com/go-jose/go-jose/)
- [ ] how to know changes, route/service/upstream/plugin_config changes, should keep the relations?
- [ ] global vars for all plugins, and the logger plugin
- [ ] mock nginx vars?
- [ ]
- [ ] route + service + upstream, merge the config
- [ ] read the conf/config-default.yaml and conf/config.yaml, and merge the config
- [ ] the plugin attr
- [ ] the plugin which modify response, how?
- [ ] plugins
  - cors
  - basic_auth
  - file-logger
  - [ ] [redirect](https://apisix.apache.org/docs/apisix/plugins/redirect/)
  - [ ] [real-ip](https://apisix.apache.org/docs/apisix/plugins/real-ip/)


