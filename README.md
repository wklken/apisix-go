# apisix-go

This is an `apisix` implemented via Go

just a toy project for now, not for production use.

no lint, no tests, no docs! I will use any libs, just for fun!

I will try to implement the `apisix` features one by one.

Build in public!

Let's see how far I can go.

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



## TODO

- [ ] how to know changes, route/service/upstream/plugin_config changes, should keep the relations?
- [ ] global vars for all plugins, and the logger plugin
- [ ] mock nginx vars?
- [ ] [proxy-rewrite](https://apisix.apache.org/docs/apisix/plugins/proxy-rewrite/)
- [ ] route + service + upstream, merge the config
- [ ] read the conf/config-default.yaml and conf/config.yaml, and merge the config
- [ ] the plugin attr
- [ ] the plugin which modify response, how?
- [ ] plugins
  - [ ] [redirect](https://apisix.apache.org/docs/apisix/plugins/redirect/)
  - [ ] [real-ip](https://apisix.apache.org/docs/apisix/plugins/real-ip/)


