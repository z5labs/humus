// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Message is a Kafka record as a Processor sees it.
type Message struct {
	// Topic is the topic the record was consumed from.
	Topic string

	// Partition is the partition of Topic the record was consumed from.
	Partition int32

	// Offset is the record's offset in its partition.
	Offset int64

	// Key is the record's key, nil when it has none.
	Key []byte

	// Value is the record's value, nil when it has none.
	Value []byte

	// Headers are the record's headers, in the record's order. A key may
	// repeat.
	Headers []Header

	// Timestamp is the record's timestamp.
	Timestamp time.Time
}

// Header is one header of a record.
type Header struct {
	Key   string
	Value []byte
}

// newMessage returns the Message of r.
func newMessage(r *kgo.Record) Message {
	var headers []Header
	if len(r.Headers) > 0 {
		headers = make([]Header, len(r.Headers))
		for i, h := range r.Headers {
			headers[i] = Header{Key: h.Key, Value: h.Value}
		}
	}
	return Message{
		Topic:     r.Topic,
		Partition: r.Partition,
		Offset:    r.Offset,
		Key:       r.Key,
		Value:     r.Value,
		Headers:   headers,
		Timestamp: r.Timestamp,
	}
}
