package codexadapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dac-plane/dac-plane/adapters/codex/journal"
)

func TestUploadErrorsDoNotQuoteTheCapability(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	unreachable := server.URL
	server.Close()
	a := &Adapter{config: Config{HTTPClient: http.DefaultClient}}
	for name, target := range map[string]string{
		"transport": unreachable + "/staging/key?X-Amz-Signature=capability",
		"malformed": "http://[bad/staging/key?X-Amz-Signature=capability",
	} {
		t.Run(name, func(t *testing.T) {
			err := a.upload(context.Background(), target, strings.NewReader("bytes"), journal.Artifact{MediaType: "text/plain", Size: 5})
			if err == nil {
				t.Fatal("upload succeeded")
			}
			if strings.Contains(err.Error(), "capability") {
				t.Fatalf("error quotes the capability: %v", err)
			}
		})
	}
}
