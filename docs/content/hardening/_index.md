---
title: Hardening
linkTitle: Hardening
description: >
  Making a humus service safe to run: authenticating and authorizing the
  requests it serves, then rehearsing its failures with failure injection.
type: docs
weight: 50
---

These pages cover what a service needs before it faces production, in this
order: authentication and authorization of the requests it serves, with the
`auth` package, then failure injection with the `fault` package, which fails
one request on purpose to rehearse what a real failure does.
