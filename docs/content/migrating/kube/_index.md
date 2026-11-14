---
title: Kubernetes controllers
linkTitle: Kubernetes controllers
description: >
  Guides that move a Kubernetes operator from another Go framework to humus:
  its reconcilers run on the kube module, on client-go with no
  controller-runtime, its custom resources are declared as Go values with
  their CRDs kept by a golden test, and its RBAC is written by hand.
type: docs
weight: 60
---

Each guide here moves a Kubernetes operator onto
[`kube`]({{< relref "/services/kubernetes" >}}). Its reconcilers are handed
their objects and return their status, its custom resources are declared as
Go values with typed constraints, and their CRDs are kept true to the types
by a golden test, as the
[databases controller walkthrough]({{< relref "/walkthroughs/kube/databases" >}})
does. Nothing in the controller requires controller-runtime.

- [kubebuilder]({{< relref "/migrating/kube/kubebuilder" >}})
