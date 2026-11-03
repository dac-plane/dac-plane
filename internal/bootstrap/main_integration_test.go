//go:build integration

package bootstrap

import (
	"os"
	"testing"

	testpostgres "github.com/dac-plane/dac-plane/internal/testsupport/postgres"
)

func TestMain(m *testing.M) { os.Exit(testpostgres.Main(m)) }
