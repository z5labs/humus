---
title: Migrating to humus
linkTitle: Migrating to humus
description: >
  Guides that move a service from another Go framework to humus, grouped by
  service type. Each maps the framework's concepts to humus's and moves one
  small service across in short steps, its whole process and not just its
  handlers, checked by the same test cases before and after.
type: docs
weight: 70
params:
  reading_order: false
---

These guides are for a service that runs on another Go framework. They aren't
part of the reading order: find your framework under your service's type and
start there.

Every guide:

- maps the framework's concepts to humus's, each linked to the page that
  covers it;
- moves one small service across, in short steps;
- covers the whole process, not just the handlers: `main`, settings, TLS,
  telemetry, health, shutdown and tests;
- runs one set of test cases against the service before and after, so clients
  see no change beyond the ones the guide names.

A guide's code is under `example/<type>/<name>` in the repository, beside the
walkthrough of its service type, and every sample on its page is quoted from
that code.

## The guides

The guides are listed under their service types, in the order
[Service types]({{< relref "/services" >}}) gives them.

### HTTP

- [Echo]({{< relref "/migrating/http/echo" >}}): an Echo v5 service, run on
  `httpserver` with its routes unchanged, then its routes moved onto
  `openapi/v3` a few at a time.
- [Gin]({{< relref "/migrating/http/gin" >}}): a Gin v1 service, run on
  `httpserver` with its routes unchanged, then its routes moved onto
  `openapi/v3` a few at a time.
- [Fiber]({{< relref "/migrating/http/fiber" >}}): a Fiber v3 service, run on
  `httpserver` through Fiber's adaptor with its routes unchanged, then its
  routes moved onto `openapi/v3` a few at a time.
- [Huma]({{< relref "/migrating/http/huma" >}}): a Huma v2 API, run on
  `httpserver` with its operations unchanged, then its operations moved onto
  `openapi/v3` a few at a time, until `openapi/v3`'s document describes it.

### Kafka

- [Watermill]({{< relref "/migrating/kafka/watermill" >}}): a Watermill v1
  consumer on Kafka, its handler moved onto a `queue.Processor` and its
  publishing onto a `kafka.Producer`, its retries and poison queue onto a
  retry topic and a dead-letter topic.

### RabbitMQ

- [Watermill]({{< relref "/migrating/rabbitmq/watermill" >}}): a Watermill
  v1 consumer on RabbitMQ, its handler moved onto a `queue.Processor` and its
  publishing onto a `rabbitmq.Publisher`, its topology into a definitions
  file, and its retries and poison queue onto a delay queue and a
  dead-letter exchange.

### Kubernetes controllers

- [kubebuilder]({{< relref "/migrating/kube/kubebuilder" >}}): a kubebuilder
  v4 operator, its reconciler moved onto a `kube.Reconciler` on client-go,
  with no controller-runtime, its custom resource declared as Go values with
  its CRD kept by a golden test, its finalizer onto `kube.Finalize`, and its
  RBAC written by hand.
