// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package embedded provides interfaces embedded within the rest package API.
package embedded

// Api is embedded in [github.com/z5labs/humus/rest.Api].
//
// Embed this interface in your implementation of the [github.com/z5labs/humus/rest.Api]
// if you want your users to experience a compilation error, signaling they need to update
// to your latest implementation, when the [github.com/z5labs/humus/rest.Api] interface
// is extended (which is something that can happen without a major version bump of the API package).
type Api interface {
	api()
}
