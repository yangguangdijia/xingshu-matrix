package sso

import "testing"

func TestSafeRedirect(t *testing.T) {
	c := &Config{RedirectBase: "https://matrix.example.com"}
	cases := map[string]string{
		"https://matrix.example.com/#/workbench": "https://matrix.example.com/#/workbench",
		"https://evil.example.com/":              "https://matrix.example.com/",
		"https://matrix.example.com.evil.io/":    "https://matrix.example.com/",
		"":                                       "https://matrix.example.com/",
		"https://matrix.example.com/?sso_ticket=old#/cluster": "https://matrix.example.com/#/cluster",
	}
	for in, want := range cases {
		if got := safeRedirect(c, in); got != want {
			t.Errorf("safeRedirect(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	c := &Config{Enabled: true, Issuer: "https://idp.example.com/", ClientID: "matrix", RedirectBase: "https://m.example.com"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Issuer != "https://idp.example.com" || c.Scopes == "" || c.UsernameClaim != "preferred_username" {
		t.Fatalf("normalize failed: %+v", c)
	}
	bad := &Config{Enabled: true, Issuer: "ftp://x", ClientID: "a", RedirectBase: "https://m"}
	if bad.Validate() == nil {
		t.Fatal("non-http issuer must be rejected")
	}
	off := &Config{}
	if off.Validate() != nil {
		t.Fatal("disabled config needs no fields")
	}
}
