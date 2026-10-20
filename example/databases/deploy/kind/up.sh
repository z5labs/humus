#!/usr/bin/env bash
set -euo pipefail

dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cluster=${KIND_CLUSTER_NAME:-kind}
namespace=databases

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if ! kind get clusters | grep -x "$cluster" >/dev/null; then
  kind create cluster --name "$cluster" --wait 2m
fi

postgres_password=$(openssl rand -hex 24)

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 \
  -subj "/CN=databases kind CA" \
  -keyout "$tmp/ca.key" -out "$tmp/ca.crt" 2>/dev/null

openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -subj "/CN=databases" \
  -keyout "$tmp/probes.key" -out "$tmp/probes.csr" 2>/dev/null

cat >"$tmp/probes.ext" <<EXT
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = serverAuth
subjectAltName = DNS:databases, DNS:databases.$namespace, DNS:databases.$namespace.svc, DNS:databases.$namespace.svc.cluster.local
EXT

openssl x509 -req -days 30 -in "$tmp/probes.csr" \
  -CA "$tmp/ca.crt" -CAkey "$tmp/ca.key" -CAserial "$tmp/ca.srl" -CAcreateserial \
  -extfile "$tmp/probes.ext" -out "$tmp/probes.crt" 2>/dev/null

cat "$tmp/probes.crt" "$tmp/probes.key" >"$tmp/probes.pem"
printf '%s' "$postgres_password" >"$tmp/postgres-password"
printf 'postgres://postgres:%s@postgres.%s.svc:5432/postgres?sslmode=disable' "$postgres_password" "$namespace" >"$tmp/admin-dsn"

k() {
  kubectl --context "kind-$cluster" --namespace "$namespace" "$@"
}

secret() {
  kubectl create secret generic "$1" --namespace "$namespace" --dry-run=client -o yaml "${@:2}"
}

{
  kubectl kustomize --load-restrictor LoadRestrictionsNone "$dir"
  echo ---
  secret postgres --from-file=password="$tmp/postgres-password"
  echo ---
  secret databases-postgres-admin --from-file=dsn="$tmp/admin-dsn"
  echo ---
  secret databases-probes --from-file=tls.pem="$tmp/probes.pem"
} >"$tmp/databases.yaml"

kind load docker-image databases --name "$cluster"
k apply -f "$tmp/databases.yaml"

k rollout restart deployment/postgres
k rollout status deployment/postgres --timeout 5m
k rollout restart deployment/databases
k rollout status deployment/databases --timeout 5m
