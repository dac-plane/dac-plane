package docstest

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/dac-plane/dac-plane/internal/provision"
)

const (
	cliPage   = repoRoot + "/docs/guides/reference/cli.md"
	mainUsage = repoRoot + "/cmd/dac-plane/main.go"
)

var (
	listedName = regexp.MustCompile(`^  ([a-z][a-z-]*)\s`)
	headingFor = regexp.MustCompile("^(#{2,3}) `([a-z][a-z-]*)`$")
)

// TestCommandLineReferenceMatchesTheUsage fails when the binary's usage text
// names a command or provisioning operation the reference has no section for,
// or the reference has a section for one the binary does not accept.
func TestCommandLineReferenceMatchesTheUsage(t *testing.T) {
	source, err := os.ReadFile(mainUsage)
	if err != nil {
		t.Fatal(err)
	}
	commands := listed(t, string(source), "commands:")
	operations := listed(t, provision.Usage, "operations:")

	page, err := os.ReadFile(cliPage)
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string][]string{}
	for line := range strings.Lines(string(page)) {
		if match := headingFor.FindStringSubmatch(strings.TrimRight(line, "\n")); match != nil {
			documented[match[1]] = append(documented[match[1]], match[2])
		}
	}
	compare(t, "command", commands, documented["##"])
	compare(t, "provisioning operation", operations, documented["###"])
}

// listed returns the names indented by two spaces in the block that follows
// the line header, up to the first blank line.
func listed(t *testing.T, text, header string) []string {
	t.Helper()
	_, block, found := strings.Cut(text, header+"\n")
	if !found {
		t.Fatalf("usage text has no %q block", header)
	}
	block, _, _ = strings.Cut(block, "\n\n")
	var names []string
	for line := range strings.Lines(block) {
		if match := listedName.FindStringSubmatch(line); match != nil {
			names = append(names, match[1])
		}
	}
	if len(names) == 0 {
		t.Fatalf("usage text lists nothing under %q", header)
	}
	return names
}

func compare(t *testing.T, kind string, accepted, documented []string) {
	t.Helper()
	for _, name := range accepted {
		if !slices.Contains(documented, name) {
			t.Errorf("dac-plane accepts the %s %q, which docs/guides/reference/cli.md has no section for", kind, name)
		}
	}
	for _, name := range documented {
		if !slices.Contains(accepted, name) {
			t.Errorf("docs/guides/reference/cli.md documents the %s %q, which dac-plane does not accept", kind, name)
		}
	}
}
