package aiops

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/app"
)

func TestPolicyChangeWindow(t *testing.T) {
	loc := policyLoc()
	// 2026-11-23 is a Monday.
	at := func(day, h, mi int) time.Time { return time.Date(2026, 11, day, h, mi, 0, 0, loc) }
	win := func(s string) Policy {
		return Policy{ChangeWindow: PolicyChangeWindow{Enabled: true, CronLike: s}}
	}
	cases := []struct {
		name string
		pol  Policy
		t    time.Time
		want bool
	}{
		{"disabled always true", Policy{}, at(29, 3, 0), true},
		{"disabled ignores garbage", Policy{ChangeWindow: PolicyChangeWindow{CronLike: "garbage"}}, at(29, 3, 0), true},
		{"mon 10:00 in", win("Mon-Fri 09:00-21:00"), at(23, 10, 0), true},
		{"start inclusive", win("Mon-Fri 09:00-21:00"), at(23, 9, 0), true},
		{"end exclusive", win("Mon-Fri 09:00-21:00"), at(23, 21, 0), false},
		{"before", win("Mon-Fri 09:00-21:00"), at(23, 8, 59), false},
		{"fri in", win("Mon-Fri 09:00-21:00"), at(27, 20, 59), true},
		{"sat out", win("Mon-Fri 09:00-21:00"), at(28, 10, 0), false},
		{"utc input converted", win("Mon-Fri 09:00-21:00"), time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC), true},
		{"utc input outside", win("Mon-Fri 09:00-21:00"), time.Date(2026, 11, 23, 0, 30, 0, 0, time.UTC), false},
		{"list days", win("Sat,Sun 00:00-24:00"), at(29, 23, 59), true},
		{"list days out", win("Sat,Sun 00:00-24:00"), at(30, 0, 0), false},
		{"wrap range", win("Fri-Mon 10:00-12:00"), at(29, 11, 0), true},
		{"wrap range out", win("Fri-Mon 10:00-12:00"), at(25, 11, 0), false},
		{"full names", win("monday-friday 09:00-18:00"), at(24, 12, 0), true},
		{"daily", win("* 01:00-02:00"), at(29, 1, 30), true},
		{"overnight late", win("Fri 22:00-06:00"), at(27, 23, 0), true},
		{"overnight early next day", win("Fri 22:00-06:00"), at(28, 5, 0), true},
		{"overnight wrong day", win("Fri 22:00-06:00"), at(27, 5, 0), false},
		{"multi windows", win("Mon-Fri 09:00-12:00; Sat 10:00-11:00"), at(28, 10, 30), true},
		{"bad format false", win("Mon-Fri"), at(23, 10, 0), false},
		{"bad day false", win("Xyz 09:00-21:00"), at(23, 10, 0), false},
		{"bad time false", win("Mon-Fri 9-21"), at(23, 10, 0), false},
		{"bad hour false", win("Mon-Fri 09:00-25:00"), at(23, 10, 0), false},
		{"empty false", win(""), at(23, 10, 0), false},
		{"equal times false", win("Mon 09:00-09:00"), at(23, 9, 0), false},
	}
	for _, tc := range cases {
		if got := inChangeWindow(tc.pol, tc.t); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestPolicyValidate(t *testing.T) {
	cases := []struct {
		name string
		p    Policy
		ok   bool
	}{
		{"zero ok", Policy{}, true},
		{"full ok", Policy{
			AutoInvestigate: PolicyAutoInvestigate{Enabled: true, Severities: []string{"Critical", "warning"}, Envs: []string{"prod"}},
			AutoExecute:     PolicyAutoExecute{Enabled: true, MaxPerHour: 5},
			ChangeWindow:    PolicyChangeWindow{Enabled: true, CronLike: "Mon-Fri 09:00-21:00"},
			ModelID:         "m1"}, true},
		{"bad severity", Policy{AutoInvestigate: PolicyAutoInvestigate{Severities: []string{"fatal"}}}, false},
		{"investigate without severity", Policy{AutoInvestigate: PolicyAutoInvestigate{Enabled: true}}, false},
		{"empty env", Policy{AutoInvestigate: PolicyAutoInvestigate{Envs: []string{" "}}}, false},
		{"exec cap zero", Policy{AutoExecute: PolicyAutoExecute{Enabled: true}}, false},
		{"exec cap negative", Policy{AutoExecute: PolicyAutoExecute{MaxPerHour: -1}}, false},
		{"exec cap too big", Policy{AutoExecute: PolicyAutoExecute{MaxPerHour: 101}}, false},
		{"bad window", Policy{ChangeWindow: PolicyChangeWindow{Enabled: true, CronLike: "weekdays"}}, false},
		{"window enabled empty", Policy{ChangeWindow: PolicyChangeWindow{Enabled: true}}, false},
		{"model too long", Policy{ModelID: strings.Repeat("x", 129)}, false},
	}
	for _, tc := range cases {
		p := tc.p
		err := policyValidate(&p)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err=%v", tc.name, err)
		}
	}
	p := Policy{AutoInvestigate: PolicyAutoInvestigate{Severities: []string{" Critical "}}}
	_ = policyValidate(&p)
	if p.AutoInvestigate.Severities[0] != "critical" {
		t.Errorf("severity not normalised: %q", p.AutoInvestigate.Severities[0])
	}
}

func TestPolicyDefaultJSON(t *testing.T) {
	m := &Module{}
	p, err := m.loadPolicy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.AutoInvestigate.Enabled || p.AutoExecute.Enabled || p.ChangeWindow.Enabled {
		t.Fatal("default must be all off")
	}
	b, _ := json.Marshal(p)
	for _, k := range []string{`"auto_investigate":{"enabled":false,"severities":[],"envs":[]}`,
		`"auto_execute":{"enabled":false,"max_per_hour":0}`, `"change_window":{"enabled":false,"cron_like":""}`, `"model_id":""`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("json %s missing %s", b, k)
		}
	}
}

func TestPolicyOnboardValidateAndRedact(t *testing.T) {
	cases := []struct {
		name string
		in   policyOnboardInput
		ok   bool
	}{
		{"password ok", policyOnboardInput{Address: "10.0.0.5", Env: "test", Username: "root", Password: "p"}, true},
		{"key ok", policyOnboardInput{Address: "db-1.internal", Port: 2222, Env: "test", Username: "ops", PrivateKey: "-----BEGIN"}, true},
		{"both creds", policyOnboardInput{Address: "10.0.0.5", Env: "test", Username: "root", Password: "p", PrivateKey: "k"}, false},
		{"no creds", policyOnboardInput{Address: "10.0.0.5", Env: "test", Username: "root"}, false},
		{"no address", policyOnboardInput{Env: "test", Username: "root", Password: "p"}, false},
		{"bad address", policyOnboardInput{Address: "a b;rm", Env: "test", Username: "root", Password: "p"}, false},
		{"bad port", policyOnboardInput{Address: "10.0.0.5", Port: 70000, Env: "test", Username: "root", Password: "p"}, false},
		{"no env", policyOnboardInput{Address: "10.0.0.5", Username: "root", Password: "p"}, false},
		{"no user", policyOnboardInput{Address: "10.0.0.5", Env: "test", Password: "p"}, false},
	}
	for _, tc := range cases {
		in := tc.in
		err := policyValidateOnboard(&in)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err=%v", tc.name, err)
		}
		if err == nil && (in.Port == 0 || in.Name == "") {
			t.Errorf("%s: defaults not filled %+v", tc.name, in)
		}
	}
	if got := policyRedact("auth failed for S3cr3t!", "S3cr3t!", ""); strings.Contains(got, "S3cr3t!") {
		t.Errorf("redact: %s", got)
	}
	v := policyAssetView(app.AssetRef{AssetID: 1, Name: "h"})
	b, _ := json.Marshal(v)
	if strings.Contains(strings.ToLower(string(b)), "password") || strings.Contains(string(b), "private") {
		t.Errorf("asset view leaks credential fields: %s", b)
	}
}
