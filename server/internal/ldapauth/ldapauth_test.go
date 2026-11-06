package ldapauth

import "testing"

func TestFilterEscapes(t *testing.T) {
	c := Config{UserFilter: "(uid={username})"}
	if got := c.Filter("a*)(uid=*"); got != `(uid=a\2a\29\28uid=\2a)` {
		t.Fatalf("filter not escaped: %s", got)
	}
}

func TestValidate(t *testing.T) {
	c := Config{Enabled: true, URL: "ldap://ldap.example.com:389", BaseDN: "dc=example,dc=com"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Config{
		{Enabled: true, URL: "http://x", BaseDN: "dc=x"},
		{Enabled: true, URL: "ldap://x:389", BaseDN: ""},
		{Enabled: true, URL: "ldap://x:389", BaseDN: "dc=x", UserFilter: "(uid=fixed)"},
		{Enabled: true, URL: "ldap://x:389", BaseDN: "dc=x", DefaultRole: "admin"},
	} {
		b := bad
		if b.Validate() == nil {
			t.Fatalf("expected rejection: %+v", b)
		}
	}
}

func TestRolesFor(t *testing.T) {
	c := Config{GroupRoles: map[string]string{"cn=ops,ou=groups,dc=x": "operator", "Dev": "developer"}}
	got := c.RolesFor([]string{"CN=ops,ou=groups,dc=x", "ou=dev,ou=groups,dc=x", "cn=other,dc=x"})
	if len(got) != 2 || got[0] != "developer" || got[1] != "operator" {
		t.Fatalf("roles: %v", got)
	}
}
