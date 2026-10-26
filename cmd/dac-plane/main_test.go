package main

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/internal/platform/migrations"
)

func TestRunRefusesInvalidCommandsBeforeReadingConfiguration(t *testing.T) {
	t.Setenv("DAC_DATABASE_URL", "")
	for name, args := range map[string][]string{
		"unknown command":      {"launch"},
		"serve with arguments": {"serve", "now"},
		"migrate without verb": {"migrate"},
		"migrate unknown verb": {"migrate", "sideways"},
		"down-to without":      {"migrate", "down-to"},
		"down-to non-number":   {"migrate", "down-to", "eleven"},
		"down-to current":      {"migrate", "down-to", strconv.FormatInt(migrations.CurrentVersion, 10)},
		"down-to negative":     {"migrate", "down-to", "-1"},
		"health without probe": {"health"},
		"health unknown probe": {"health", "deep"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != 2 {
				t.Fatalf("run(%v) = %d, want 2; stderr %q", args, code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "usage: dac-plane") {
				t.Fatalf("stderr %q carries no usage", stderr.String())
			}
		})
	}
}

func TestHealthFailsWhenNothingListens(t *testing.T) {
	t.Setenv("DAC_OPS_ADDR", "127.0.0.1:1")
	var stderr bytes.Buffer
	if code := runHealth([]string{"live"}, &stderr); code != 1 {
		t.Fatalf("runHealth() = %d, want 1; stderr %q", code, stderr.String())
	}
}
