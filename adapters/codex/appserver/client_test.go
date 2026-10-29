package appserver

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestReaderRoutesResponsesRequestsAndNotifications(t *testing.T) {
	response := make(chan Message, 1)
	client := &Client{pending: map[string]chan Message{"1": response}, done: make(chan struct{}), Notifications: make(chan Message, 2), Requests: make(chan Message, 2)}
	input := strings.NewReader("{\"id\":1,\"result\":{}}\n{\"method\":\"turn/started\",\"params\":{}}\n{\"id\":17,\"method\":\"item/commandExecution/requestApproval\",\"params\":{}}\n")
	go client.read(input)
	select {
	case got := <-response:
		if string(got.Result) != "{}" {
			t.Fatalf("result=%s", got.Result)
		}
	case <-time.After(time.Second):
		t.Fatal("response was not routed")
	}
	select {
	case got := <-client.Notifications:
		if got.Method != "turn/started" {
			t.Fatalf("notification=%s", got.Method)
		}
	case <-time.After(time.Second):
		t.Fatal("notification was not routed")
	}
	select {
	case got := <-client.Requests:
		if got.Method != "item/commandExecution/requestApproval" || string(got.ID) != "17" {
			t.Fatalf("request=%+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("request was not routed")
	}
	<-client.done
}

func TestMalformedNativeMessageFailsPendingCalls(t *testing.T) {
	pending := make(chan Message, 1)
	client := &Client{pending: map[string]chan Message{"1": pending}, done: make(chan struct{}), Notifications: make(chan Message, 1), Requests: make(chan Message, 1)}
	go client.read(strings.NewReader("not-json\n"))
	<-client.done
	if _, ok := <-pending; ok {
		t.Fatal("pending response stayed open")
	}
	client.mu.Lock()
	err := client.err
	client.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "decode app-server message") {
		t.Fatalf("err=%v", err)
	}
}

func TestStderrRedactsProviderCredential(t *testing.T) {
	var output bytes.Buffer
	secret := "credential-that-must-not-leak"
	client := &Client{logger: slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})), secrets: []string{secret}}
	client.logStderr(strings.NewReader("request failed with " + secret + "\n"))
	if strings.Contains(output.String(), secret) || !strings.Contains(output.String(), "[REDACTED]") {
		t.Fatalf("redacted log=%s", output.String())
	}
}

func TestTurnFailureLogKeepsOnlySafeDiagnosticFields(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logTurnFailure(logger, []byte(`{"turn":{"status":"failed","error":{"codexErrorInfo":"other","httpStatusCode":401,"message":"sensitive prompt and credential"}}}`))
	got := output.String()
	if !strings.Contains(got, `"codex_error_info":"other"`) {
		t.Fatalf("missing failure diagnostics: %s", got)
	}
	if strings.Contains(got, "sensitive prompt") || strings.Contains(got, "credential") || strings.Contains(got, "http_status") {
		t.Fatalf("failure diagnostic redaction failed: %s", got)
	}
}

func TestStartRejectsUnsafeConfigurationBeforeExecuting(t *testing.T) {
	cases := []Config{{Binary: "missing", Home: t.TempDir(), Provider: "camer.bad", Model: "m", BaseURL: "https://example.com/v1", APIKeyEnv: "KEY", APIKey: "secret"}, {Binary: "missing", Home: t.TempDir(), Provider: "camer", Model: "m", BaseURL: "http://example.com/v1", APIKeyEnv: "KEY", APIKey: "secret"}}
	for _, config := range cases {
		client, err := Start(t.Context(), config)
		if client != nil || err == nil {
			t.Fatalf("config=%+v client=%v err=%v", config, client, err)
		}
	}
}
