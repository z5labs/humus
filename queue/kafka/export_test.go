// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"

	"github.com/twmb/franz-go/pkg/kgo"
)

// KgoLevel returns the Level of the kgo.Logger svc hands its client.
func KgoLevel(svc *Service) kgo.LogLevel {
	return kgoLogger{ctx: context.Background(), logger: svc.kgoLogger}.Level()
}

// CloseProducer closes p's client, for a test that has no App to run p's
// OnStop function.
func CloseProducer(p *Producer) {
	p.client.Close()
}
