package console

import (
	"sync"

	"rymesh/internal/i18n"
)

// The English catalog is web/i18n/en.json, shared with the page, the Mac app
// and the Android app. The server keeps its messages in Chinese (the message
// ids); the page translates what it shows, and command-line output follows
// the terminal's locale.

var enCatalog = sync.OnceValue(func() *i18n.Catalog {
	b, err := webFS.ReadFile("web/i18n/en.json")
	if err != nil {
		return nil
	}
	c, _ := i18n.Parse(b)
	return c
})

var cliLang = sync.OnceValue(i18n.EnvLang)

// cliCatalog is the catalog for command-line output, nil for Chinese.
func cliCatalog() *i18n.Catalog {
	if cliLang() == "zh" {
		return nil
	}
	return enCatalog()
}

// T formats a command-line message in the terminal's language.
func T(format string, args ...any) string {
	return cliCatalog().Sprintf(format, args...)
}

// Tr translates a finished message (an error, say) for the terminal.
func Tr(msg string) string {
	return cliCatalog().Translate(msg)
}

// TranslateEN translates a message into English, for the Android app's own
// strings (mobile.Translate).
func TranslateEN(msg string) string {
	return enCatalog().Translate(msg)
}
