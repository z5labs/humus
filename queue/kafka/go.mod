module github.com/z5labs/humus/queue/kafka

go 1.27.0

// The core module is always the one in the same commit, so the kafka module
// is built and tested against the humus it ships beside.
replace github.com/z5labs/humus => ../..

require (
	github.com/stretchr/testify v1.12.1
	github.com/twmb/franz-go v1.22.2
	github.com/z5labs/humus v0.0.0-00010101000000-000000000000
)

require go.yaml.in/yaml/v3 v3.0.5 // indirect
