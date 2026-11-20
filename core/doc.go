// Package core defines the protocol types, JSON-RPC 2.0 framing, version and
// era constants, capability and result types, error codes, and the shared
// interfaces that every other package in the SDK programs against.
//
// It sits at the root of the import graph: it must not import client, server,
// transport, middleware, auth, or any adapter, and it must not require a
// third-party module.
package core
