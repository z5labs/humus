#!/usr/bin/env bash
set -euo pipefail

dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
module=$(cd "$dir/../.." && pwd)
cluster=${KIND_CLUSTER_NAME:-kind}
namespace=polls

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if ! kind get clusters | grep -x "$cluster" >/dev/null; then
  kind create cluster --name "$cluster" --wait 2m
fi

postgres_password=$(openssl rand -hex 24)
polls_password=$(openssl rand -hex 24)

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 \
  -subj "/CN=polls kind CA" \
  -keyout "$tmp/ca.key" -out "$tmp/ca.crt" 2>/dev/null

issue() {
  openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -subj "/CN=$2" \
    -keyout "$tmp/$1.key" -out "$tmp/$1.csr" 2>/dev/null

  cat >"$tmp/$1.ext" <<EXT
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = $3
subjectAltName = DNS:$2${4:+, $4}
EXT

  openssl x509 -req -days 30 -in "$tmp/$1.csr" \
    -CA "$tmp/ca.crt" -CAkey "$tmp/ca.key" -CAserial "$tmp/ca.srl" -CAcreateserial \
    -extfile "$tmp/$1.ext" -out "$tmp/$1.crt" 2>/dev/null
}

service_names() {
  printf 'DNS:%s.%s, DNS:%s.%s.svc, DNS:%s.%s.svc.cluster.local' "$1" "$namespace" "$1" "$namespace" "$1" "$namespace"
}

issue polls polls serverAuth "$(service_names polls), DNS:localhost, IP:127.0.0.1"
issue postgres postgres serverAuth "$(service_names postgres)"
issue cases cases clientAuth

printf 'postgres://polls:%s@postgres:5432/polls?sslmode=verify-full&sslrootcert=/etc/polls/postgres-ca/ca.crt' "$polls_password" >"$tmp/polls-url"
printf '%s' "$postgres_password" >"$tmp/postgres-password"

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

{
  kubectl kustomize "$dir"
  echo ---
  tls polls-tls polls
  echo ---
  secret polls-database --from-file=url="$tmp/polls-url"
  echo ---
  secret polls-postgres-ca --from-file=ca.crt="$tmp/ca.crt"
  echo ---
  secret polls-fault-ca --from-file=ca.crt="$tmp/ca.crt"
  echo ---
  secret polls-cases --from-file=tls.crt="$tmp/cases.crt" --from-file=tls.key="$tmp/cases.key" --from-file=ca.crt="$tmp/ca.crt"
  echo ---
  secret postgres --from-file=password="$tmp/postgres-password"
  echo ---
  tls postgres-tls postgres
} >"$tmp/polls.yaml"

kind load docker-image polls --name "$cluster"
k apply -f "$tmp/polls.yaml"

k rollout restart deployment/postgres
k rollout status deployment/postgres --timeout 5m

{
  cat <<SQL
CREATE ROLE polls_owner NOLOGIN;
CREATE ROLE polls LOGIN PASSWORD '$polls_password';
CREATE DATABASE polls OWNER polls_owner;
REVOKE ALL ON DATABASE polls FROM PUBLIC;
GRANT CONNECT ON DATABASE polls TO polls;
\connect polls
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO polls;
SET ROLE polls_owner;
SQL
  cat "$module/store/schema.sql"
  cat <<SQL

GRANT SELECT, INSERT ON polls, options, votes TO polls;
SQL
} | k exec -i deployment/postgres -- psql -v ON_ERROR_STOP=1 -U postgres -d postgres

k rollout restart deployment/polls
k rollout status deployment/polls --timeout 5m
