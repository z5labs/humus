#!/usr/bin/env bash
set -euo pipefail

kafka() {
  "/opt/kafka/bin/$1" --bootstrap-server kafka:9093 --command-config /etc/kafka-admin/config/admin.properties "${@:2}"
}

until kafka kafka-topics.sh --list >/dev/null; do
  sleep 2
done

for topic in orders.events orders.status orders.dlq orders.retry; do
  kafka kafka-topics.sh --create --if-not-exists --topic "$topic" --partitions 12 --replication-factor 1
done

acl() {
  kafka kafka-acls.sh --add --allow-principal "User:CN=$1" "${@:2}"
}

acl orderstatus --operation Read --group orderstatus
acl orderstatus --operation Read --topic orders.events --topic orders.retry
acl orderstatus --operation Write --topic orders.status --topic orders.retry --topic orders.dlq

acl cases --operation Write --topic orders.events
acl cases --operation Read --topic orders.status --topic orders.retry --topic orders.dlq
acl cases --operation Describe --group orderstatus

acl otel-collector --operation Describe --group orderstatus
acl otel-collector --operation Describe --operation DescribeConfigs --topic orders.events --topic orders.retry
