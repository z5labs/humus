#!/usr/bin/env bash
set -euo pipefail

dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
module=$(cd "$dir/../.." && pwd)
cluster=${KIND_CLUSTER_NAME:-kind}
namespace=shoppingcart

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if ! kind get clusters | grep -x "$cluster" >/dev/null; then
  kind create cluster --name "$cluster" --wait 2m
fi

postgres_password=$(openssl rand -hex 24)
carts_owner_password=$(openssl rand -hex 24)
cart_password=$(openssl rand -hex 24)
catalog_owner_password=$(openssl rand -hex 24)
catalog_password=$(openssl rand -hex 24)
grafana_password=$(openssl rand -hex 24)
export KEYSTORE_PASSWORD
KEYSTORE_PASSWORD=$(openssl rand -hex 24)

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 \
  -subj "/CN=shoppingcart kind CA" \
  -keyout "$tmp/ca.key" -out "$tmp/ca.crt" 2>/dev/null

issue() {
  openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -subj "/CN=$2" \
    -keyout "$tmp/$1.key" -out "$tmp/$1.csr" 2>/dev/null

  cat >"$tmp/$1.ext" <<EXT
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = $3
subjectAltName = DNS:$2, DNS:$2.$namespace, DNS:$2.$namespace.svc, DNS:$2.$namespace.svc.cluster.local${4:+, $4}
EXT

  openssl x509 -req -days 30 -in "$tmp/$1.csr" \
    -CA "$tmp/ca.crt" -CAkey "$tmp/ca.key" -CAserial "$tmp/ca.srl" -CAcreateserial \
    -extfile "$tmp/$1.ext" -out "$tmp/$1.crt" 2>/dev/null
}

issue cart cart serverAuth "DNS:localhost, IP:127.0.0.1"
issue cart-catalog-client cart clientAuth
issue catalog catalog serverAuth "DNS:localhost, IP:127.0.0.1"
issue postgres postgres serverAuth
issue wiremock wiremock serverAuth

openssl pkcs12 -export -name wiremock \
  -inkey "$tmp/wiremock.key" -in "$tmp/wiremock.crt" -certfile "$tmp/ca.crt" \
  -passout env:KEYSTORE_PASSWORD -out "$tmp/keystore.p12"

openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$tmp/signing.key" 2>/dev/null
go -C "$module" run ./cmd/carttoken jwks -key "$tmp/signing.key" >"$tmp/jwks.json"

url() {
  printf 'postgres://%s:%s@postgres:5432/%s?sslmode=verify-full&sslrootcert=/etc/%s/ca/ca.crt' "$@" >"$tmp/$1-url"
}

url carts_owner "$carts_owner_password" carts cart
url cart "$cart_password" carts cart
url catalog_owner "$catalog_owner_password" catalog catalog
url catalog "$catalog_password" catalog catalog

printf '%s' "$postgres_password" >"$tmp/postgres-password"
printf '%s' "$KEYSTORE_PASSWORD" >"$tmp/keystore-password"
printf '%s' "$grafana_password" >"$tmp/grafana-password"

k() {
  kubectl --context "kind-$cluster" --namespace "$namespace" "$@"
}

secret() {
  kubectl create secret generic "$1" --namespace "$namespace" --dry-run=client -o yaml "${@:2}"
}

tls() {
  kubectl create secret tls "$1" --namespace "$namespace" --dry-run=client -o yaml \
    --cert="$tmp/$2.crt" --key="$tmp/$2.key"
}

configmap() {
  kubectl create configmap "$1" --namespace "$namespace" --dry-run=client -o yaml "${@:2}"
}

{
  kubectl kustomize "$dir"
  echo ---
  tls cart-tls cart
  echo ---
  tls cart-catalog-client cart-catalog-client
  echo ---
  secret cart-database --from-file=url="$tmp/cart-url"
  echo ---
  secret cart-database-owner --from-file=url="$tmp/carts_owner-url"
  echo ---
  secret cart-ca --from-file=ca.crt="$tmp/ca.crt"
  echo ---
  tls catalog-tls catalog
  echo ---
  secret catalog-client-ca --from-file=ca.crt="$tmp/ca.crt"
  echo ---
  secret catalog-database --from-file=url="$tmp/catalog-url"
  echo ---
  secret catalog-database-owner --from-file=url="$tmp/catalog_owner-url"
  echo ---
  secret catalog-ca --from-file=ca.crt="$tmp/ca.crt"
  echo ---
  secret postgres --from-file=password="$tmp/postgres-password"
  echo ---
  tls postgres-tls postgres
  echo ---
  secret wiremock --from-file=keystore.p12="$tmp/keystore.p12" --from-file=password="$tmp/keystore-password"
  echo ---
  secret token-signing-key --from-file=key.pem="$tmp/signing.key"
  echo ---
  configmap wiremock-files --from-file=jwks.json="$tmp/jwks.json"
  echo ---
  secret grafana-admin --from-literal=username=admin --from-file=password="$tmp/grafana-password"
} >"$tmp/shoppingcart.yaml"

kind load docker-image shoppingcart --name "$cluster"
k apply -f "$tmp/shoppingcart.yaml"

k rollout restart deployment/postgres deployment/wiremock
k rollout status deployment/postgres --timeout 5m
k rollout status deployment/wiremock --timeout 5m

k exec -i deployment/postgres -- psql -v ON_ERROR_STOP=1 -U postgres -d postgres <<SQL
CREATE ROLE carts_owner LOGIN PASSWORD '$carts_owner_password';
CREATE ROLE cart LOGIN PASSWORD '$cart_password';
CREATE DATABASE carts OWNER carts_owner;
REVOKE ALL ON DATABASE carts FROM PUBLIC;
GRANT CONNECT ON DATABASE carts TO cart;
CREATE ROLE catalog_owner LOGIN PASSWORD '$catalog_owner_password';
CREATE ROLE catalog LOGIN PASSWORD '$catalog_password';
CREATE DATABASE catalog OWNER catalog_owner;
REVOKE ALL ON DATABASE catalog FROM PUBLIC;
GRANT CONNECT ON DATABASE catalog TO catalog;
SQL

k rollout restart deployment/grafana deployment/catalog deployment/cart
for d in loki tempo mimir otel-collector grafana catalog cart; do
  k rollout status "deployment/$d" --timeout 5m
done
