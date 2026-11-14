// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command humus is the Memcached operator the kubebuilder migration guide ends with: a Controller on humus's kube module.
package main

import (
	"github.com/z5labs/humus"
)

func main() {
	humus.Main(Init, humus.WithTelemetry(buildTelemetry))
}
