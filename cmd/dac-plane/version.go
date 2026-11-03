package main

import (
	"fmt"
	"io"

	"github.com/dac-plane/dac-plane/adapterkit/wire"
	"github.com/dac-plane/dac-plane/internal/platform/migrations"
)

// The release build sets these (ADR-0018).
var (
	version  = "dev"
	revision = "unknown"
)

func runVersion(stdout io.Writer) int {
	_, _ = fmt.Fprintf(stdout, "version=%s\nrevision=%s\nschema_min=%d\nschema_current=%d\nprotocol=%d\n",
		version, revision, migrations.MinServeVersion, migrations.CurrentVersion, wire.ProtocolVersion)
	return 0
}
