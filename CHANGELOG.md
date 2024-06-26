## still in progress

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
- [x] plugin uri-blocker [apisix doc: uri-blocker](https://apisix.apache.org/zh/docs/apisix/plugins/uri-blocker/) 2024-03-29
- [x] plugin limit-count local [limit-count](https://apisix.apache.org/zh/docs/apisix/plugins/limit-count/) => [ulule/limiter](https://github.com/ulule/limiter) 2024-03-30
- [x] plugin limit-count redis  2024-03-31
- [x] plugin api-breaker basic logical [api-breaker](https://apisix.apache.org/zh/docs/apisix/plugins/api-breaker/) 2024-04-02
- [x] plugin api-breaker reset the response 2024-04-03
- [x] plugin gzip [gzip](https://apisix.apache.org/zh/docs/apisix/plugins/gzip/) 2024-04-03
- [x] plugin referer-restriction [referer-restriction](https://apisix.apache.org/zh/docs/apisix/plugins/referer-restriction/)  => [gobwas/glob](https://github.com/gobwas/glob) / [Shopify/goreferrer](github.com/Shopify/goreferrer) 2024-04-03
- [x] plugin ua-restriction [ua-restriction](https://apisix.apache.org/zh/docs/apisix/plugins/ua-restriction/) 2024-04-04
- [x] ip-restriction [ip-restriction](https://apisix.apache.org/zh/docs/apisix/plugins/ip-restriction/) [ipfilter](https://github.com/jpillora/ipfilter) 2024-04-05
- [x] basic-auth [basic-auth](https://apisix.apache.org/zh/docs/apisix/plugins/basic-auth/) 2024-04-07
- [x] key-auth [key-auth](https://apisix.apache.org/zh/docs/apisix/plugins/key-auth/) 2024-04-10
- [x] logger fields getter 2024-04-11
- [x] file-logger [file-logger](https://apisix.apache.org/zh/docs/apisix/plugins/file-logger/) 2024-04-12/13/15
- [x] nginx and apisix vars 2024-04-13
- [x] request vars 2024-04-14
- [x] add apisix vars into ctx 2024-04-15
- [x] plugin cors [cors](https://apisix.apache.org/zh/docs/apisix/plugins/cors/) 2024-04-16
- [x] plugin request-validateion [request-validateion](https://apisix.apache.org/zh/docs/apisix/plugins/request-validateion/) 2024-04-18
- [x] plugin fault-injection [fault-injection](https://apisix.apache.org/zh/docs/apisix/plugins/fault-injection/) 2024-04-19
- [x] plugin redirect [redirect](https://apisix.apache.org/zh/docs/apisix/plugins/redirect/) 2024-04-20
- [x] plugin csrf [csrf](https://apisix.apache.org/zh/docs/apisix/plugins/csrf/) 2024-04-21
- [x] plugin prometheus [prometheus](https://apisix.apache.org/zh/docs/apisix/plugins/prometheus/) 2024-04-21
- [x] standalone mode file watcher 2024-04-22
- [x] global rules 2024-04-23
- [x] plugin_config_id in route 2024-04-28
- [x] attach the consumer 2024-04-29
- [x] plugin consumer-restriction [consumer-restriction](https://apisix.apache.org/zh/docs/apisix/plugins/consumer-restriction/) 2024-04-29
- [x] plugin http-logger [http-logger](https://apisix.apache.org/zh/docs/apisix/plugins/http-logger/) 2024-04-30
- [x] plugin udp-logger [udp-logger](https://apisix.apache.org/zh/docs/apisix/plugins/udp-logger/) 2024-05-01
- [x] plugin sys-logger [syslog](https://apisix.apache.org/zh/docs/apisix/plugins/syslog/) 2024-05-01
- [x] plugin tcp-logger [tcp-logger](https://apisix.apache.org/zh/docs/apisix/plugins/tcp-logger/) 2024-05-02
- [x] refactor base.BaseLoggerPlugin / file-logger with buffered 2024-05-02
- [x] refactor store/getter.go 2024-05-03
- [x] admin api dir 2024-05-06
- [x] Dockerfile 2024-05-07
- [x] plugin elasticsearch-logger [elasticsearch-logger](https://apisix.apache.org/zh/docs/apisix/plugins/elasticsearch-logger/) 2024-05-09
- [x] support plugin share the same client 2024-05-25