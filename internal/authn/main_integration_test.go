//go:build integration

package authn

import (
	"os"
	"testing"

	testpostgres "github.com/AssahBismarkabah/dac-plane/internal/testsupport/postgres"
)

func TestMain(m *testing.M) { os.Exit(testpostgres.Main(m)) }
