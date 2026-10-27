package settings

import "testing"

func TestModuleManagedKey(t *testing.T) {
	tests := []struct {
		key     string
		managed bool
	}{
		{key: "platform.domain", managed: true},
		{key: "provision.registry_mirror", managed: true},
		{key: "notify", managed: true},
		{key: "notify.webhook", managed: true},
		{key: "notify.email.smtp", managed: true},
		{key: "public.site_name", managed: false},
		{key: "notifications.webhook", managed: false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := moduleManagedKey(tt.key); got != tt.managed {
				t.Fatalf("moduleManagedKey(%q) = %t, want %t", tt.key, got, tt.managed)
			}
		})
	}
}
