#!/usr/bin/env bash
set -euo pipefail

dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cluster=${KIND_CLUSTER_NAME:-kind}
namespace=orderstatus

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if ! kind get clusters | grep -x "$cluster" >/dev/null; then
  kind create cluster --name "$cluster" --wait 2m
fi

postgres_password=$(openssl rand -hex 24)
orderstatus_password=$(openssl rand -hex 24)

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 \
  -subj "/CN=orderstatus kind CA" \
  -keyout "$tmp/ca.key" -out "$tmp/ca.crt" 2>/dev/null

issue() {
  openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -subj "/CN=$1" \
    -keyout "$tmp/$1.key" -out "$tmp/$1.csr" 2>/dev/null

  cat >"$tmp/$1.ext" <<EXT
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = serverAuth, clientAuth
subjectAltName = DNS:$1, DNS:$1.$namespace, DNS:$1.$namespace.svc, DNS:$1.$namespace.svc.cluster.local${2:+, $2}
EXT

  openssl x509 -req -days 30 -in "$tmp/$1.csr" \
    -CA "$tmp/ca.crt" -CAkey "$tmp/ca.key" -CAserial "$tmp/ca.srl" -CAcreateserial \
    -extfile "$tmp/$1.ext" -out "$tmp/$1.crt" 2>/dev/null

  cat "$tmp/$1.key" "$tmp/$1.crt" >"$tmp/$1.pem"
}

issue kafka DNS:localhost
issue admin
issue postgres
issue orderstatus
issue cases

printf '%s' "$postgres_password" >"$tmp/postgres-password"
printf '%s' "$orderstatus_password" >"$tmp/orderstatus-password"

k() {
  kubectl --context "kind-$cluster" --namespace "$namespace" "$@"
}

secret() {
  kubectl create secret generic "$1" --namespace "$namespace" --dry-run=client -o yaml "${@:2}"
}

{
  kubectl kustomize --load-restrictor LoadRestrictionsNone "$dir"
  echo ---
  secret kafka-tls --from-file=keystore.pem="$tmp/kafka.pem" --from-file=ca.crt="$tmp/ca.crt"
  echo ---
  secret kafka-admin-tls --from-file=tls.pem="$tmp/admin.pem" --from-file=ca.crt="$tmp/ca.crt"
  echo ---
  secret postgres --from-file=password="$tmp/postgres-password"
  echo ---
  secret postgres-tls --from-file=tls.crt="$tmp/postgres.crt" --from-file=tls.key="$tmp/postgres.key"
  echo ---
  secret orderstatus-kafka --from-file=tls.pem="$tmp/orderstatus.pem" --from-file=ca.crt="$tmp/ca.crt"
  echo ---
  secret orderstatus-postgres --from-file=password="$tmp/orderstatus-password" --from-file=ca.crt="$tmp/ca.crt"
  echo ---
  secret orderstatus-probes --from-file=tls.pem="$tmp/orderstatus.pem"
  echo ---
  secret cases-kafka --from-file=tls.pem="$tmp/cases.pem" --from-file=ca.crt="$tmp/ca.crt"
} >"$tmp/orderstatus.yaml"

kind load docker-image orderstatus --name "$cluster"
k delete job kafka-topics --ignore-not-found
k apply -f "$tmp/orderstatus.yaml"

k rollout restart deployment/kafka deployment/postgres
k delete job kafka-topics
k rollout status deployment/kafka --timeout 5m
k rollout status deployment/postgres --timeout 5m
k apply -f "$tmp/orderstatus.yaml"

k exec -i deployment/postgres -- psql -v ON_ERROR_STOP=1 -U postgres -d orders <<SQL
CREATE ROLE orderstatus LOGIN PASSWORD '$orderstatus_password';
GRANT SELECT, INSERT, UPDATE ON order_events, order_status TO orderstatus;
SQL

k wait job/kafka-topics --for=condition=Complete --timeout 5m

k rollout restart deployment/orderstatus
k rollout status deployment/orderstatus --timeout 5m
