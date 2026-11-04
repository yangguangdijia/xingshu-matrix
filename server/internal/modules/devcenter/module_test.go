package devcenter

import "testing"

func TestInputCheck(t *testing.T) {
	s := func(v string) *string { return &v }
	in := input{Title: s("  登录支持企业 SSO  "), Status: s("accepting"), Priority: s("P1")}
	if err := in.check("requirements", true); err != nil || *in.Title != "登录支持企业 SSO" {
		t.Fatalf("valid input rejected: %v", err)
	}
	if (&input{Title: s("x"), Status: s("accepting")}).check("bugs", true) == nil {
		t.Fatal("requirement status must be invalid for bugs")
	}
	if (&input{Title: s("x"), Priority: s("P9")}).check("bugs", true) == nil {
		t.Fatal("bad priority must be rejected")
	}
	if (&input{}).check("tests", true) == nil {
		t.Fatal("title required on create")
	}
	if (&input{Status: s("passed")}).check("tests", false) != nil {
		t.Fatal("patch without title must be allowed")
	}
	if (&input{Title: s("x"), Data: []byte(`[1]`)}).check("tests", true) == nil {
		t.Fatal("data must be an object")
	}
}
