#!/usr/bin/env bash
set -euo pipefail

dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cluster=${KIND_CLUSTER_NAME:-kind}
namespace=billing

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if ! kind get clusters | grep -x "$cluster" >/dev/null; then
  kind create cluster --name "$cluster" --wait 2m
fi

password=$(openssl rand -hex 24)
api_key=$(openssl rand -hex 32)
export KEYSTORE_PASSWORD
KEYSTORE_PASSWORD=$(openssl rand -hex 24)

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 \
  -subj "/CN=billing kind CA" \
  -keyout "$tmp/ca.key" -out "$tmp/ca.crt" 2>/dev/null

openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -subj "/CN=wiremock" \
  -keyout "$tmp/wiremock.key" -out "$tmp/wiremock.csr" 2>/dev/null

cat >"$tmp/wiremock.ext" <<EOF
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = serverAuth
subjectAltName = DNS:wiremock, DNS:wiremock.$namespace, DNS:wiremock.$namespace.svc, DNS:wiremock.$namespace.svc.cluster.local
EOF

openssl x509 -req -days 30 -in "$tmp/wiremock.csr" \
  -CA "$tmp/ca.crt" -CAkey "$tmp/ca.key" -CAserial "$tmp/ca.srl" -CAcreateserial \
  -extfile "$tmp/wiremock.ext" -out "$tmp/wiremock.crt" 2>/dev/null

openssl pkcs12 -export -name wiremock \
  -inkey "$tmp/wiremock.key" -in "$tmp/wiremock.crt" -certfile "$tmp/ca.crt" \
  -passout env:KEYSTORE_PASSWORD -out "$tmp/keystore.p12"

printf '%s' "$password" >"$tmp/password"
printf 'postgres://billing:%s@postgres:5432/billing?sslmode=disable' "$password" >"$tmp/url"
printf '%s' "$api_key" >"$tmp/api-key"
printf '%s' "$KEYSTORE_PASSWORD" >"$tmp/keystore-password"

k() {
  kubectl --context "kind-$cluster" "$@"
}

secret() {
  kubectl create secret generic "$1" --namespace "$namespace" --dry-run=client -o yaml "${@:2}"
}

{
  kubectl kustomize --load-restrictor LoadRestrictionsNone "$dir"
  echo ---
  secret postgres --from-file=password="$tmp/password"
  echo ---
  secret wiremock --from-file=keystore.p12="$tmp/keystore.p12" --from-file=password="$tmp/keystore-password"
  echo ---
  secret billing-database --from-file=url="$tmp/url"
  echo ---
  secret billing-payments --from-file=api-key="$tmp/api-key"
  echo ---
  kubectl create configmap billing-payments-ca --namespace "$namespace" --dry-run=client -o yaml \
    --from-file=ca.crt="$tmp/ca.crt"
} >"$tmp/billing.yaml"

kind load docker-image billing --name "$cluster"
k apply -f "$tmp/billing.yaml"

k rollout restart deployment/postgres deployment/wiremock --namespace "$namespace"
k rollout status deployment/postgres --namespace "$namespace" --timeout 5m
k rollout status deployment/wiremock --namespace "$namespace" --timeout 5m
