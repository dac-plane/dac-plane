package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigurationErrorsNameTheSetting(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("s3-test-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"DAC_S3_ENDPOINT": "https://store:8443", "DAC_S3_ACCESS_KEY_ID": "adapter", "DAC_S3_SECRET_ACCESS_KEY_FILE": secret, "DAC_S3_STAGING_BUCKET": "dac-staging", "DAC_S3_OBJECTS_BUCKET": "dac-objects"}
	getenv := func(name string) string { return env[name] }
	if _, err := storeFromEnv(getenv); err != nil {
		t.Fatal(err)
	}
	delete(env, "DAC_S3_OBJECTS_BUCKET")
	if _, err := storeFromEnv(getenv); err == nil || !strings.Contains(err.Error(), "DAC_S3_OBJECTS_BUCKET") {
		t.Fatalf("missing bucket: %v", err)
	}
	env["DAC_S3_OBJECTS_BUCKET"], env["DAC_S3_ENDPOINT"] = "dac-objects", "http://store:8333"
	if _, err := storeFromEnv(getenv); err == nil {
		t.Fatal("plain HTTP endpoint accepted")
	}
	env["DAC_S3_STAGING_DAYS"] = "0"
	if _, err := days(getenv, "DAC_S3_STAGING_DAYS", 2); err == nil {
		t.Fatal("zero staging days accepted")
	}
	if err := run([]string{"delete"}, getenv); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
}
