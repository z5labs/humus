#!/usr/bin/env bash
set -euo pipefail

dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
module=$(cd "$dir/../.." && pwd)
cluster=${KIND_CLUSTER_NAME:-kind}
namespace=wallet

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if ! kind get clusters | grep -x "$cluster" >/dev/null; then
  kind create cluster --name "$cluster" --wait 2m
fi

postgres_password=$(openssl rand -hex 24)
ledger_password=$(openssl rand -hex 24)
grafana_password=$(openssl rand -hex 24)
export KEYSTORE_PASSWORD
KEYSTORE_PASSWORD=$(openssl rand -hex 24)

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 \
  -subj "/CN=wallet kind CA" \
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

  cat "$tmp/$1.crt" "$tmp/$1.key" >"$tmp/$1.pem"
}

issue wallet wallet serverAuth "DNS:localhost, IP:127.0.0.1"
issue wallet-ledger-client wallet clientAuth
issue ledger ledger serverAuth "DNS:localhost, IP:127.0.0.1"
issue postgres postgres serverAuth
issue wiremock wiremock serverAuth
issue cases cases clientAuth

openssl pkcs12 -export -name wiremock \
  -inkey "$tmp/wiremock.key" -in "$tmp/wiremock.crt" -certfile "$tmp/ca.crt" \
  -passout env:KEYSTORE_PASSWORD -out "$tmp/keystore.p12"

openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$tmp/signing.key" 2>/dev/null
go -C "$module" run ./cmd/walletctl jwks -key "$tmp/signing.key" >"$tmp/jwks.json"

printf '%s' "$postgres_password" >"$tmp/postgres-password"
printf '%s' "$ledger_password" >"$tmp/ledger-password"
printf '%s' "$KEYSTORE_PASSWORD" >"$tmp/keystore-password"
printf '%s' "$grafana_password" >"$tmp/grafana-password"

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
  kubectl kustomize "$dir"
  echo ---
  secret wallet-tls --from-file=tls.pem="$tmp/wallet.pem"
  echo ---
  secret wallet-ledger-client --from-file=tls.pem="$tmp/wallet-ledger-client.pem"
  echo ---
  secret wallet-ca --from-file=ledger.crt="$tmp/ca.crt" --from-file=token-keys.crt="$tmp/ca.crt"
  echo ---
  secret ledger-tls --from-file=tls.pem="$tmp/ledger.pem"
  echo ---
  secret ledger-ca --from-file=client-ca.crt="$tmp/ca.crt" --from-file=postgres.crt="$tmp/ca.crt"
  echo ---
  secret ledger-postgres --from-file=password="$tmp/ledger-password"
  echo ---
  secret postgres --from-file=password="$tmp/postgres-password"
  echo ---
  secret postgres-tls --from-file=tls.crt="$tmp/postgres.crt" --from-file=tls.key="$tmp/postgres.key"
  echo ---
  secret wiremock --from-file=keystore.p12="$tmp/keystore.p12" --from-file=password="$tmp/keystore-password"
  echo ---
  configmap wiremock-files --from-file=jwks.json="$tmp/jwks.json"
  echo ---
  secret cases-tls --from-file=tls.pem="$tmp/cases.pem" --from-file=ca.crt="$tmp/ca.crt"
  echo ---
  secret token-signing-key --from-file=key.pem="$tmp/signing.key"
  echo ---
  secret grafana-admin --from-literal=username=admin --from-file=password="$tmp/grafana-password"
} >"$tmp/wallet.yaml"

kind load docker-image wallet --name "$cluster"
k apply -f "$tmp/wallet.yaml"

k rollout restart deployment/postgres deployment/wiremock
k rollout status deployment/postgres --timeout 5m
k rollout status deployment/wiremock --timeout 5m

{
  cat <<SQL
CREATE ROLE ledger_owner;
CREATE ROLE ledger LOGIN PASSWORD '$ledger_password';
CREATE DATABASE ledger OWNER ledger_owner;
REVOKE ALL ON DATABASE ledger FROM PUBLIC;
GRANT CONNECT ON DATABASE ledger TO ledger;
\connect ledger
SET ROLE ledger_owner;
SQL
  cat "$module/ledger/store/schema.sql"
  cat <<SQL
GRANT SELECT, INSERT, UPDATE ON accounts, transfers TO ledger;
SQL
} | k exec -i deployment/postgres -- psql -v ON_ERROR_STOP=1 -U postgres -d postgres

k rollout restart deployment/grafana deployment/ledger deployment/wallet
for d in loki tempo mimir otel-collector grafana ledger wallet; do
  k rollout status "deployment/$d" --timeout 5m
done
