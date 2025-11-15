// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import "log/slog"

// GroupIDAttr returns a slog attribute for the Kafka consumer group ID.
func GroupIDAttr(groupID string) slog.Attr {
	return slog.String("messaging.consumer.group.name", groupID)
}

// TopicAttr returns a slog attribute for the Kafka topic.
func TopicAttr(topic string) slog.Attr {
	return slog.String("messaging.destination.name", topic)
}

// PartitionAttr returns a slog attribute for the Kafka partition.
func PartitionAttr(partition int32) slog.Attr {
	return slog.Int64("messaging.destination.partition.id", int64(partition))
}

// OffsetAttr returns a slog attribute for the Kafka offset.
func OffsetAttr(offset int64) slog.Attr {
	return slog.Int64("messaging.kafka.offset", offset)
}
