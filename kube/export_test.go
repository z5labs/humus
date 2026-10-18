// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"k8s.io/client-go/dynamic"
	coordinationv1 "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/metadata"
)

// SetClients replaces the clients c reaches the API server with, so a unit
// test hands it client-go's fakes: dyn for its resource, meta for the
// resources it owns, and leases for its Lease.
func SetClients(c *Controller, dyn dynamic.Interface, meta metadata.Interface, leases coordinationv1.LeasesGetter) {
	c.dyn = dyn
	c.meta = meta
	c.leases = leases
}
