#!/usr/bin/env bash
set -euo pipefail

dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cluster=${KIND_CLUSTER_NAME:-kind}
namespace=billing
context="kind-$cluster"

tmp=$(mktemp -d)
pids=()
cleanup() {
  for pid in "${pids[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  rm -rf "$tmp"
}
trap cleanup EXIT

k() {
  kubectl --context "$context" --namespace "$namespace" "$@"
}

forward() {
  k port-forward "service/$1" ":$2" >"$tmp/$1" 2>&1 &
  pids+=("$!")
}

port() {
  for _ in $(seq 60); do
    local p
    p=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$tmp/$1" | head -n 1)
    if [ -n "$p" ]; then
      echo "$p"
      return
    fi
    sleep 1
  done
  cat "$tmp/$1" >&2
  return 1
}

forward postgres 5432
forward wiremock 443
postgres_port=$(port postgres)
wiremock_port=$(port wiremock)

password=$(k get secret postgres --output jsonpath='{.data.password}' | base64 --decode)

go -C "$dir/../.." test -count=1 -race -tags functional "$@" ./functional/ -args \
  -kube-context "$context" \
  -namespace "$namespace" \
  -postgres "postgres://billing:$password@127.0.0.1:$postgres_port/billing?sslmode=disable" \
  -postgres-address postgres:5432 \
  -wiremock "https://127.0.0.1:$wiremock_port"
