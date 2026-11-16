module github.com/z5labs/humus/tools/flaky

go 1.27.0

require (
	github.com/stretchr/testify v1.12.1
	github.com/z5labs/bedrock v0.26.0
	github.com/z5labs/humus/tools/ci v0.0.0-00010101000000-000000000000
	golang.org/x/sys v0.50.0
)

require go.yaml.in/yaml/v3 v3.0.6 // indirect

replace github.com/z5labs/humus/tools/ci => ../ci
