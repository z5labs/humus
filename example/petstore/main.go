// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"bytes"
	_ "embed"

	"github.com/z5labs/humus/example/petstore/app"

	"github.com/z5labs/humus"
)

//go:embed config.yaml
var configBytes []byte

func main() {
	humus.Run(bytes.NewReader(configBytes), app.Init)
}
