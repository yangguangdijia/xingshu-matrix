package rbac

import (
	"reflect"
	"testing"
)

func TestIntersectMatchesBoth(t *testing.T) {
	cases := []struct {
		a, b []string
		want []string
	}{
		{[]string{"*"}, []string{"host:read"}, []string{"host:read"}},
		{[]string{"host:*"}, []string{"*:read"}, []string{"host:read"}},
		{[]string{"host:*", "user:read"}, []string{"host:read", "user:manage"}, []string{"host:read"}},
		{[]string{"*"}, []string{"*"}, []string{"*"}},
		{[]string{"*:*"}, []string{"*"}, []string{"*"}},
		{[]string{"host:read"}, []string{"cluster:read"}, []string{}},
		{nil, []string{"host:read"}, []string{}},
		{[]string{"bad"}, []string{"host:read"}, []string{}},
	}
	for _, c := range cases {
		if got := Intersect(c.a, c.b); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Intersect(%v,%v)=%v want %v", c.a, c.b, got, c.want)
		}
	}
	// 性质：Match(Intersect(a,b),p) == Match(a,p) && Match(b,p)
	grants := [][]string{{"*"}, {"host:*"}, {"*:read"}, {"host:read", "user:manage"}, {"cluster:exec"}, {}}
	perms := []string{"host:read", "host:write", "user:manage", "user:read", "cluster:exec", "cluster:read"}
	for _, a := range grants {
		for _, b := range grants {
			in := Intersect(a, b)
			for _, p := range perms {
				if Match(in, p) != (Match(a, p) && Match(b, p)) {
					t.Errorf("property broken for a=%v b=%v p=%s (in=%v)", a, b, p, in)
				}
			}
		}
	}
}

func TestCovers(t *testing.T) {
	cases := []struct {
		granted []string
		code    string
		want    bool
	}{
		{[]string{"*"}, "*", true},
		{[]string{"*"}, "host:*", true},
		{[]string{"host:*"}, "host:read", true},
		{[]string{"host:*"}, "host:*", true},
		{[]string{"host:read"}, "host:*", false},
		{[]string{"host:read", "host:write"}, "host:*", false}, // 单条授权必须完整覆盖
		{[]string{"*:read"}, "host:read", true},
		{[]string{"*:read"}, "*:read", true},
		{[]string{"*:read"}, "*", false},
		{[]string{"host:*"}, "*:read", false},
		{nil, "host:read", false},
		{[]string{"host:read"}, "garbage", false},
	}
	for _, c := range cases {
		if got := Covers(c.granted, c.code); got != c.want {
			t.Errorf("Covers(%v,%q)=%v want %v", c.granted, c.code, got, c.want)
		}
	}
}

func TestValidCodeAndMatch(t *testing.T) {
	for _, ok := range []string{"*", "host:read", "host:*", "*:read", "k8s.pod:exec"} {
		if !ValidCode(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "host", "Host:read", "a:b:c", "host:", ":read", "host read"} {
		if ValidCode(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
	if !Match([]string{"host:*"}, "host:read") || Match([]string{"host:read"}, "host:write") {
		t.Error("match")
	}
}
