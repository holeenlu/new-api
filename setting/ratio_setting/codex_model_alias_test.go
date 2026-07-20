package ratio_setting

import "testing"

func TestCanonicalCodexModelAlias(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "sol alias", input: "sol", want: "gpt-5.6-sol"},
		{name: "terra alias", input: "terra", want: "gpt-5.6-terra"},
		{name: "luna alias", input: "luna", want: "gpt-5.6-luna"},
		{name: "case and space insensitive", input: "  SOL  ", want: "gpt-5.6-sol"},
		{name: "already canonical passes through", input: "gpt-5.6-sol", want: "gpt-5.6-sol"},
		{name: "unknown model passes through", input: "gpt-4o", want: "gpt-4o"},
		{name: "empty passes through", input: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalCodexModelAlias(tc.input); got != tc.want {
				t.Fatalf("CanonicalCodexModelAlias(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
