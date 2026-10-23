---
title: gRPC
linkTitle: gRPC
description: >
  Serving gRPC with the grpcserver package, from the grpc module.
type: docs
weight: 20
---

A gRPC service registers its generated services on `grpcserver`'s Service
and serves them over TLS, which
[Serving gRPC]({{< relref "/services/grpc/serving" >}}) covers.

The [wallet walkthrough]({{< relref "/walkthroughs/grpc/wallet" >}}) builds a
production-ready gRPC service step by step, and its code is
[`example/grpc/wallet`](https://github.com/z5labs/humus/tree/main/example/grpc/wallet).
