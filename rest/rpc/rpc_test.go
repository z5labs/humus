// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rpc

// Tests for the rpc package have been migrated to integration tests
// that use the handlers with rest.Handle().
//
// The rpc package now provides handler implementations (ReturnJsonHandler,
// ConsumeJsonHandler, ConsumerHandler, ProducerHandler) that implement
// the rest.Handler interface, and error handling is now managed by the
// rest package via rest.ErrorHandler and rest.HttpResponseWriter.
