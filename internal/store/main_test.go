package store

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Tests must not leave vault keys in the developer's keychain.
	UseKeychain = false
	os.Exit(m.Run())
}
