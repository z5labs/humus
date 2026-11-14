module github.com/z5labs/humus/example/http/huma/start

go 1.27.0

replace github.com/z5labs/humus/example/http/huma/cases => ../cases

require (
	github.com/danielgtaylor/huma/v2 v2.40.0
	github.com/stretchr/testify v1.12.1
	github.com/z5labs/humus/example/http/huma/cases v0.0.0-00010101000000-000000000000
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/cobra v1.10.2 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	go.yaml.in/yaml/v3 v3.0.6 // indirect
)
