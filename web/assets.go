// Package web bundles the local interface without a runtime frontend toolchain.
package web

import "embed"

// Assets contains only the interface and its third-party notices.
//
//go:embed *.html *.css *.js *.txt
var Assets embed.FS
