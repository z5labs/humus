#!/usr/bin/env bash
set -euo pipefail

dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cluster=${KIND_CLUSTER_NAME:-kind}
namespace=mailer

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if ! kind get clusters | grep -x "$cluster" >/dev/null; then
  kind create cluster --name "$cluster" --wait 2m
fi

mailer_password=$(openssl rand -hex 24)
cases_password=$(openssl rand -hex 24)
mailrequest_password=$(openssl rand -hex 24)
collector_password=$(openssl rand -hex 24)
grafana_password=$(openssl rand -hex 24)
api_key=$(openssl rand -hex 32)
export KEYSTORE_PASSWORD
KEYSTORE_PASSWORD=$(openssl rand -hex 24)

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 \
  -subj "/CN=mailer kind CA" \
  -keyout "$tmp/ca.key" -out "$tmp/ca.crt" 2>/dev/null

issue() {
  openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -subj "/CN=$1" \
    -keyout "$tmp/$1.key" -out "$tmp/$1.csr" 2>/dev/null

  cat >"$tmp/$1.ext" <<EOF
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = serverAuth
subjectAltName = DNS:$1, DNS:$1.$namespace, DNS:$1.$namespace.svc, DNS:$1.$namespace.svc.cluster.local
EOF

  openssl x509 -req -days 30 -in "$tmp/$1.csr" \
    -CA "$tmp/ca.crt" -CAkey "$tmp/ca.key" -CAserial "$tmp/ca.srl" -CAcreateserial \
    -extfile "$tmp/$1.ext" -out "$tmp/$1.crt" 2>/dev/null
}

issue rabbitmq
issue wiremock
issue mailer

openssl pkcs12 -export -name wiremock \
  -inkey "$tmp/wiremock.key" -in "$tmp/wiremock.crt" -certfile "$tmp/ca.crt" \
  -passout env:KEYSTORE_PASSWORD -out "$tmp/keystore.p12"

cat "$tmp/mailer.crt" "$tmp/mailer.key" >"$tmp/probes.pem"
printf '%s' "$mailer_password" >"$tmp/mailer-password"
printf '%s' "$cases_password" >"$tmp/cases-password"
printf '%s' "$mailrequest_password" >"$tmp/mailrequest-password"
printf '%s' "$collector_password" >"$tmp/collector-password"
printf '%s' "$grafana_password" >"$tmp/grafana-password"
printf '%s' "$api_key" >"$tmp/api-key"
printf '%s' "$KEYSTORE_PASSWORD" >"$tmp/keystore-password"

k() {
  kubectl --context "kind-$cluster" --namespace "$namespace" "$@"
}

secret() {
  kubectl create secret generic "$1" --namespace "$namespace" --dry-run=client -o yaml "${@:2}"
}

configmap() {
  kubectl create configmap "$1" --namespace "$namespace" --dry-run=client -o yaml "${@:2}"
}

{
  kubectl kustomize --load-restrictor LoadRestrictionsNone "$dir"
  echo ---
  secret rabbitmq-tls --from-file=tls.crt="$tmp/rabbitmq.crt" --from-file=tls.key="$tmp/rabbitmq.key" \
    --from-file=ca.crt="$tmp/ca.crt"
  echo ---
  secret wiremock --from-file=keystore.p12="$tmp/keystore.p12" --from-file=password="$tmp/keystore-password"
  echo ---
  secret mailer-rabbitmq --from-file=password="$tmp/mailer-password"
  echo ---
  secret mailer-provider --from-file=api-key="$tmp/api-key"
  echo ---
  secret mailer-probes --from-file=tls.pem="$tmp/probes.pem"
  echo ---
  secret cases-rabbitmq --from-literal=username=cases --from-file=password="$tmp/cases-password"
  echo ---
  secret mailrequest-rabbitmq --from-literal=username=mailrequest --from-file=password="$tmp/mailrequest-password"
  echo ---
  secret otel-collector-rabbitmq --from-file=password="$tmp/collector-password"
  echo ---
  secret grafana-admin --from-literal=username=admin --from-file=password="$tmp/grafana-password"
  echo ---
  configmap mailer-rabbitmq-ca --from-file=ca.crt="$tmp/ca.crt"
  echo ---
  configmap mailer-provider-ca --from-file=ca.crt="$tmp/ca.crt"
} >"$tmp/mailer.yaml"

kind load docker-image mailer --name "$cluster"
k apply -f "$tmp/mailer.yaml"

k rollout restart deployment/rabbitmq deployment/wiremock
k rollout status deployment/rabbitmq --timeout 5m
k rollout status deployment/wiremock --timeout 5m

rabbitmqctl() {
  k exec -i deployment/rabbitmq -- rabbitmqctl "$@"
}

login() {
  rabbitmqctl add_user "$1" <"$tmp/$1-password"
  rabbitmqctl set_permissions --vhost / "$1" "$2" "$3" "$4"
}

rabbitmqctl await_startup
rabbitmqctl delete_user guest
rabbitmqctl import_definitions /etc/rabbitmq-definitions/definitions.json
rabbitmqctl import_definitions /etc/rabbitmq-definitions/policies.json

login mailer '^$' '^mail\.(retry|dead|events)$' '^mail\.send$'
login cases '^amq\.gen-.*$' '^(mail|amq\.gen-.*)$' '^(mail\.(events|dead|retry)|amq\.gen-.*)$'
login mailrequest '^$' '^mail$' '^$'
login collector '^$' '^$' '^$'
rabbitmqctl set_user_tags collector monitoring

k rollout restart deployment/otel-collector deployment/grafana deployment/mailer
for d in loki tempo mimir otel-collector grafana mailer; do
  k rollout status "deployment/$d" --timeout 5m
done
