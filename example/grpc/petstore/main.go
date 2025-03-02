// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"bytes"
	_ "embed"

	"github.com/z5labs/humus/example/grpc/petstore/app"
	"github.com/z5labs/humus/grpc"
)

//go:embed config.yaml
var configBytes []byte

func main() {
	grpc.Run(bytes.NewReader(configBytes), app.Init)
}
