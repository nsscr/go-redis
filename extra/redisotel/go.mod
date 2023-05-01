module github.com/redis/go-redis/extra/redisotel/v9

go 1.19

replace github.com/redis/go-redis/v9 => ../..

replace github.com/redis/go-redis/extra/rediscmd/v9 => ../rediscmd

require (
	github.com/redis/go-redis/extra/rediscmd/v9 v9.0.4
	github.com/redis/go-redis/v9 v9.0.4
	go.opentelemetry.io/otel v1.15.0
	go.opentelemetry.io/otel/metric v0.38.0
	go.opentelemetry.io/otel/sdk v1.15.0
	go.opentelemetry.io/otel/trace v1.15.0
)

require (
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/go-logr/logr v1.2.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	golang.org/x/sys v0.7.0 // indirect
)
