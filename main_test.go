package console

import (
	"os"
	"testing"

	"rymesh/internal/store"
)

func TestMain(m *testing.M) {
	// Tests must not leave vault keys in the developer's keychain.
	store.UseKeychain = false
	os.Exit(m.Run())
}
