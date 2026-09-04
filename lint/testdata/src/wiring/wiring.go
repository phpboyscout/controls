// Package wiring exports a server built at package level, for the cross-package
// capture fixture.
package wiring

import "net/http"

var Srv = &http.Server{Addr: ":0"}
