// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

//go:build mistakes

package mistakes

import (
	"regexp"

	"github.com/z5labs/humus/kube"
)

var _ = kube.Rules(&spec,
	kube.Field(&spec.Size).Minimum(1),
	kube.Field(&spec.Image).Pattern(regexp.MustCompile(`^memcached:`)).Default("memcached:1.6.26-alpine3.19"),
)
