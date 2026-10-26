// Command dac-plane is the control plane's single binary (ADR-0009): serve
// runs every role of a serving process, migrate applies schema migrations,
// provision performs trusted provisioning and health probes the operations
// listener, so a container health check needs no shell.
package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `usage: dac-plane <command> [arguments]

commands:
  serve       run the Surface API, the adapter endpoint and the worker (the default)
  migrate     up | status | down-to VERSION: apply or inspect schema migrations
  provision   perform one trusted provisioning operation
  health      live | ready: probe the local operations listener`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return runServe()
	}
	switch args[0] {
	case "serve":
		if len(args) != 1 {
			return usageError(stderr, "serve takes no arguments")
		}
		return runServe()
	case "migrate":
		return runMigrate(args[1:], stdout, stderr)
	case "provision":
		return runProvision(args[1:], stdout, stderr)
	case "health":
		return runHealth(args[1:], stderr)
	case "-h", "-help", "--help", "help":
		_, _ = fmt.Fprintln(stdout, usage)
		return 0
	}
	return usageError(stderr, fmt.Sprintf("unknown command %q", args[0]))
}

func usageError(stderr io.Writer, message string) int {
	_, _ = fmt.Fprintf(stderr, "dac-plane: %s\n\n%s\n", message, usage)
	return 2
}
