package settings

import (
	"encoding/base64"
	"testing"
)

func TestAppearanceValidate(t *testing.T) {
	ok := Appearance{PrimaryColor: "#0F7B6C", Logo: "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("\x89PNG...."))}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Appearance{
		{PrimaryColor: "blue"},
		{Logo: "https://x/logo.png"},
		{Logo: "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(`<svg onload="alert(1)"></svg>`))},
		{Logo: "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(`<svg><script>x</script></svg>`))},
	} {
		b := bad
		if b.Validate() == nil {
			t.Fatalf("expected rejection: %+v", b.PrimaryColor+b.Logo[:min(30, len(b.Logo))])
		}
	}
}
