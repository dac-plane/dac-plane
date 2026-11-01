package execclient

import (
	"context"
	"strings"
	"testing"

	"github.com/AssahBismarkabah/dac-plane/adapters/codex/execclient/exectest"
)

func TestSnapshotWalksTheWorkspaceAndSkipsToolDirectories(t *testing.T) {
	endpoint := exectest.New(t, "token", map[string]string{
		"/workspace/a.txt":        "hello",
		"/workspace/sub/b.txt":    "world",
		"/workspace/.git/config":  "skipped",
		"/workspace/.codex/state": "skipped",
	}).URL
	client, err := Dial(context.Background(), endpoint, "token")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	files, err := client.Snapshot(context.Background(), "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %+v", files)
	}
	want := "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if files["a.txt"].Digest != want || files["a.txt"].Size != 5 || files["sub/b.txt"].Size != 5 {
		t.Fatalf("files = %+v", files)
	}
}

func TestDialRefusesTheWrongToken(t *testing.T) {
	endpoint := exectest.New(t, "token", nil).URL
	if _, err := Dial(context.Background(), endpoint, "wrong"); err == nil {
		t.Fatal("wrong token connected")
	}
}

func TestReadFileReportsAMissingFile(t *testing.T) {
	endpoint := exectest.New(t, "token", nil).URL
	client, err := Dial(context.Background(), endpoint, "token")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.ReadFile(context.Background(), "/workspace/none"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v", err)
	}
}
