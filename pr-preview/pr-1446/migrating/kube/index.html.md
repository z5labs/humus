# Kubernetes controllers

Each guide here moves a Kubernetes operator onto
[`kube`](/humus/pr-preview/pr-1446/services/kubernetes/). Its reconcilers are handed
their objects and return their status, its custom resources are declared as
Go values with typed constraints, and their CRDs are kept true to the types
by a golden test, as the
[databases controller walkthrough](/humus/pr-preview/pr-1446/walkthroughs/kube/databases/)
does. Nothing in the controller requires controller-runtime.

- [kubebuilder](/humus/pr-preview/pr-1446/migrating/kube/kubebuilder/)
