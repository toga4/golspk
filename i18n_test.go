package main

import (
	"strings"
	"testing"
)

func TestLocalize(t *testing.T) {
	tests := []struct {
		locale string
		want   string
	}{
		{locale: "ja", want: "を参照しうるパッケージが多いため"},
		{locale: "ja-JP", want: "を参照しうるパッケージが多いため"},
		{locale: "en-US", want: "Too many packages may refer to Foo"},
		{locale: "fr", want: "Too many packages may refer to Foo"},
		{locale: "", want: "Too many packages may refer to Foo"},
	}
	for _, tt := range tests {
		t.Run(tt.locale, func(t *testing.T) {
			s := newServer(nil)
			s.setLocale(tt.locale)
			if got := s.localize(msgReferencesTruncated, "Foo", 400); !strings.Contains(got, tt.want) || !strings.Contains(got, "400") {
				t.Errorf("localize() = %q, want it to contain %q and 400", got, tt.want)
			}
		})
	}
}

func TestMessagesCoverEveryLanguage(t *testing.T) {
	for lang, table := range messages {
		for id := range messages["en"] {
			if table[id] == "" {
				t.Errorf("messages[%q] lacks message %d", lang, id)
			}
		}
	}
}
