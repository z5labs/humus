// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"

	"github.com/twmb/franz-go/pkg/kbin"
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

// Classify is the Producer's breaker classifier.
var Classify = classify

// StripRecord is svc's strip of the failure scenario of a record, m, whose
// context under its process span is ctx.
func StripRecord(svc *Service, ctx context.Context, m Message) context.Context {
	return svc.strip(ctx, m)
}

// ClientSASL is the mechanism the client is given for m.
var ClientSASL = clientSASL

// FranzGoValues are the error values franz-go's packages export, by their
// full names. TestFranzGoValues_lists_every_error_value_franz_go_exports
// keeps it complete.
var FranzGoValues = map[string]error{
	"github.com/twmb/franz-go/pkg/kgo.ErrRecordTimeout":  kgo.ErrRecordTimeout,
	"github.com/twmb/franz-go/pkg/kgo.ErrRecordRetries":  kgo.ErrRecordRetries,
	"github.com/twmb/franz-go/pkg/kgo.ErrMaxBuffered":    kgo.ErrMaxBuffered,
	"github.com/twmb/franz-go/pkg/kgo.ErrAborting":       kgo.ErrAborting,
	"github.com/twmb/franz-go/pkg/kgo.ErrClientClosed":   kgo.ErrClientClosed,
	"github.com/twmb/franz-go/pkg/kgo.ErrMaxDecompress":  kgo.ErrMaxDecompress,
	"github.com/twmb/franz-go/pkg/kbin.ErrNotEnoughData": kbin.ErrNotEnoughData,
}

// FromClient is the translation of an error from the client.
var FromClient = fromClient

// RecordError is the translation of a record's result from the client.
var RecordError = recordError
