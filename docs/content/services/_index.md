---
title: Service types
linkTitle: Service types
description: >
  The kinds of service humus runs, one section each: HTTP, gRPC, jobs, Kafka,
  RabbitMQ and Kubernetes controllers, each with its guide, its walkthrough
  and its example.
type: docs
weight: 30
---

A service type is a package whose Services an `Init` returns to `humus.Main`.
Each section here is a guide to one of them, in this order: HTTP, gRPC, jobs,
Kafka, RabbitMQ and Kubernetes controllers. Each links to its walkthrough,
which builds a production-ready service of that type step by step, and to the
walkthrough's code under `example/` on GitHub.
