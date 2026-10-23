// Package mobile is the gomobile binding the Android app uses: start the
// embedded console (a hub reaching servers over SSH) and get a login URL for
// the WebView.
package mobile

import (
	"sync"

	console "rymesh"
)

var (
	mu  sync.Mutex
	cur *console.Embedded
)

// Start runs the console with its data in dataDir (the app's files dir) and
// returns a one-time login URL. vaultKey (32 bytes from the Android Keystore)
// seals the stored SSH keys. Calling it again returns a fresh URL.
func Start(dataDir string, vaultKey []byte) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	if cur == nil {
		e, err := console.StartEmbedded(dataDir, vaultKey)
		if err != nil {
			return "", err
		}
		cur = e
	}
	return cur.LoginURL(), nil
}

// Language is the interface language chosen in Settings, "zh" or "en", or
// "" to follow the system.
func Language() string {
	mu.Lock()
	defer mu.Unlock()
	if cur == nil {
		return ""
	}
	return cur.Language()
}

// Translate puts one of the app's own messages (Chinese, the message id)
// into English.
func Translate(msg string) string {
	return console.TranslateEN(msg)
}

// Stop shuts the console down; SSH connections close with it. Agents on the
// servers keep running.
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if cur != nil {
		cur.Stop()
		cur = nil
	}
}
