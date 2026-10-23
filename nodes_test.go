package console

import (
	"testing"
)

func TestCheckDirectURL(t *testing.T) {
	for u, ok := range map[string]bool{
		"http://10.0.0.2:8899":    true,
		"http://192.168.1.5:8899": true,
		"http://100.80.1.2:8899":  true,
		"http://127.0.0.1:8899":   true,
		"http://mac.lan:8899":     true,
		"https://8.8.8.8:8899":    true,
		"http://8.8.8.8:8899":     false,
		"ftp://10.0.0.1":          false,
		"10.0.0.1:8899":           false,
	} {
		if err := checkDirectURL(u); (err == nil) != ok {
			t.Errorf("%s: %v", u, err)
		}
	}
}
