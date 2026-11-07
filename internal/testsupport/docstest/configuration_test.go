// Package docstest keeps the user guide in agreement with the code it describes.
package docstest

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	repoRoot          = "../../.."
	configurationPage = repoRoot + "/docs/guides/reference/configuration.md"
	everyAdapter      = "Every adapter"
)

// binary is one released program: the source it is built from and whether
// it reads the settings the reference lists under "Every adapter".
type binary struct {
	sources []string
	adapter bool
	// notSettings are DAC_ names in the source that an operator does not
	// set, each with the reason.
	notSettings map[string]string
}

var binaries = map[string]binary{
	"dac-plane":         {sources: []string{"cmd/dac-plane", "internal"}},
	"dac-codex-adapter": {sources: []string{"adapters/codex"}, adapter: true, notSettings: map[string]string{"DAC_CODEX_PROVIDER_API_KEY": "the variable the adapter puts the API key in for the app-server it starts"}},
	"dac-e2b-adapter":   {sources: []string{"adapters/e2b"}, adapter: true},
	"dac-s3-adapter":    {sources: []string{"adapters/s3"}, adapter: true},
}

// skippedDirs hold code that is not part of a released binary: test support,
// the deployed stack test, provider qualification runners and examples.
var skippedDirs = []string{"testsupport", "stacktest", "qualification", "examples"}

var (
	nameInSource = regexp.MustCompile(`"(DAC_[A-Z0-9_]+)"`)
	nameInRow    = regexp.MustCompile("^\\| `(DAC_[A-Z0-9_]+)` \\|")
)

// TestConfigurationReferenceMatchesTheCode fails when a released binary reads
// a DAC_ variable the configuration reference does not list for it, or when
// the reference lists one the binary does not read.
func TestConfigurationReferenceMatchesTheCode(t *testing.T) {
	documented := documentedSettings(t)
	for section := range documented {
		if _, ok := binaries[section]; !ok && section != everyAdapter {
			t.Errorf("the configuration reference lists variables under %q, which is not a released binary", section)
		}
	}
	for name, bin := range binaries {
		t.Run(name, func(t *testing.T) {
			read := readSettings(t, bin)
			listed := slices.Clone(documented[name])
			if bin.adapter {
				listed = append(listed, documented[everyAdapter]...)
			}
			for _, variable := range read {
				if !slices.Contains(listed, variable) {
					t.Errorf("%s reads %s, which docs/guides/reference/configuration.md does not list for it", name, variable)
				}
			}
			for i, variable := range listed {
				if !slices.Contains(read, variable) {
					t.Errorf("docs/guides/reference/configuration.md lists %s for %s, which does not read it", variable, name)
				}
				if slices.Contains(listed[:i], variable) {
					t.Errorf("docs/guides/reference/configuration.md lists %s for %s twice", variable, name)
				}
			}
		})
	}
}

// documentedSettings returns the variables in the first column of each
// table, by the level-two heading they appear under.
func documentedSettings(t *testing.T) map[string][]string {
	t.Helper()
	file, err := os.Open(configurationPage)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	settings := map[string][]string{}
	section := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if heading, ok := strings.CutPrefix(line, "## "); ok {
			section = strings.TrimSpace(heading)
			continue
		}
		if match := nameInRow.FindStringSubmatch(line); match != nil {
			settings[section] = append(settings[section], match[1])
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return settings
}

// readSettings returns the DAC_ string literals in a binary's non-test Go
// source, less the ones that are not settings.
func readSettings(t *testing.T, bin binary) []string {
	t.Helper()
	var names []string
	for _, source := range bin.sources {
		err := filepath.WalkDir(filepath.Join(repoRoot, source), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if slices.Contains(skippedDirs, entry.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, match := range nameInSource.FindAllStringSubmatch(string(data), -1) {
				if _, skip := bin.notSettings[match[1]]; !skip && !slices.Contains(names, match[1]) {
					names = append(names, match[1])
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(names) == 0 {
		t.Fatalf("found no DAC_ variables in %v; the source paths are wrong", bin.sources)
	}
	return names
}
