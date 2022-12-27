[![Go Report Card](https://goreportcard.com/badge/github.com/soypat/peasocket)](https://goreportcard.com/report/github.com/soypat/peasocket)
[![GoDoc](https://godoc.org/github.com/soypat/peasocket?status.svg)](https://godoc.org/github.com/soypat/peasocket)
[![codecov](https://codecov.io/gh/soypat/peasocket/branch/main/graph/badge.svg)](https://codecov.io/gh/soypat/peasocket/branch/main)

# peasocket
Little websocket implementation

## Highlights
* Empty `go.mod` file.
* Lockless base implementation
* Dead-simple Client implementation
    * No channels
    * No goroutines
    * 1 Mutex on state and another on `Tx` to auto-reply pings.

* Ready for use in embedded systems
* Tinygo compatible.
* Base implementation is very simple, 1000 lines of code excluding `Client`. This makes debugging and testing easy. 
* Easy to extend. Exposes all websocket functionality.

## `cmd/peacho` Command
A echoing client. Connects to a server and echoes back any messages received.