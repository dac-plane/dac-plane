package migrations

import "testing"

func TestNewProviderRejectsNilDatabase(t *testing.T) {
	provider, err := NewProvider(nil, nil)
	if provider != nil {
		t.Fatal("NewProvider() provider is non-nil for nil database")
	}
	if err == nil {
		t.Fatal("NewProvider() error = nil, want error")
	}
}
