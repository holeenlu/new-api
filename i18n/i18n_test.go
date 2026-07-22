package i18n

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultLangFromEnv(t *testing.T) {
	tests := []struct {
		raw      string
		wantLang string
		wantOK   bool
	}{
		{raw: "", wantOK: false},
		{raw: "  ", wantOK: false},
		{raw: "fr", wantOK: false},
		{raw: "en", wantLang: LangEn, wantOK: true},
		{raw: "EN-US", wantLang: LangEn, wantOK: true},
		{raw: "zh", wantLang: LangZhCN, wantOK: true},
		{raw: "zh-CN", wantLang: LangZhCN, wantOK: true},
		{raw: "zh-Hans", wantLang: LangZhCN, wantOK: true},
		{raw: "zh-TW", wantLang: LangZhTW, wantOK: true},
		{raw: "zh-Hant", wantLang: LangZhTW, wantOK: true},
	}
	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			lang, ok := defaultLangFromEnv(test.raw)
			require.Equal(t, test.wantOK, ok)
			require.Equal(t, test.wantLang, lang)
		})
	}
}
