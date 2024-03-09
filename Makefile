

build:
	go build -o apisix

serve: build
	./apisix -c config.yaml 

