// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package humustest runs a whole humus App inside a test.
//
// bedrocktest supplies the process an App runs on. humustest supplies what
// a humus App needs beyond it: a [Host], whose in-memory network the App
// listens on and the test dials. The network opens no real socket and
// starts no goroutines, so an App on it runs inside a testing/synctest
// bubble.
//
// The rules of the network follow a real one. A name and port can be held
// once, and a listener on every name (an empty host, 0.0.0.0 or ::)
// conflicts with any other listener on its port. Names are compared as
// written and never resolved, so localhost and 127.0.0.1 are different
// names. A Dial with no listener fails at once with ECONNREFUSED; a test
// waits for the App to listen before it dials, with synctest.Wait inside a
// bubble or with [Host.Listening] anywhere.
//
// The design is Proposal: humustest:
// https://github.com/z5labs/humus/discussions/633
package humustest
