package main

import (
	"fmt"
	"strings"
)

// msgID identifies a message that the server shows to the user.
type msgID int

const (
	msgReferencesTruncated msgID = iota
	msgImplementationsTruncated
)

// messages holds the user-facing messages for each supported language. English is the fallback.
var messages = map[string]map[msgID]string{
	"en": {
		msgReferencesTruncated:      "golspk: Too many packages may refer to %s, so only %d packages per module were searched. The results are partial.",
		msgImplementationsTruncated: "golspk: Too many packages may implement %s, so only %d packages per module were searched. The results are partial.",
	},
	"ja": {
		msgReferencesTruncated:      "golspk: %s を参照しうるパッケージが多いため、モジュールごとに %d パッケージだけを調べました。結果は一部です。",
		msgImplementationsTruncated: "golspk: %s の実装を持ちうるパッケージが多いため、モジュールごとに %d パッケージだけを調べました。結果は一部です。",
	},
}

// setLocale stores the display language from the locale of the initialize parameters, such as "ja" or "en-US".
func (s *server) setLocale(locale string) {
	lang, _, _ := strings.Cut(strings.ToLower(locale), "-")
	if _, ok := messages[lang]; !ok {
		lang = "en"
	}
	s.mu.Lock()
	s.lang = lang
	s.mu.Unlock()
}

// localize returns the message id in the display language of the client, formatted with args.
func (s *server) localize(id msgID, args ...any) string {
	s.mu.Lock()
	lang := s.lang
	s.mu.Unlock()
	format, ok := messages[lang][id]
	if !ok {
		format = messages["en"][id]
	}
	return fmt.Sprintf(format, args...)
}
