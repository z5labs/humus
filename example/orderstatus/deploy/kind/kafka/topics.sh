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
  kafka kafka-acls.sh --add --allow-principal User:CN=orderstatus "$@"
}

acl --operation Read --group orderstatus
acl --operation Read --topic orders.events --topic orders.retry
acl --operation Write --topic orders.status --topic orders.retry --topic orders.dlq
