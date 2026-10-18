// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

//go:build mistakes

package mistakes

import "github.com/z5labs/humus/kube"

var _ = kube.Rules(&spec, kube.Field(&spec.Size).Minimun(1))
