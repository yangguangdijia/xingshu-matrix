package mail

import (
	"strings"
	"testing"
	"time"
)

func TestBuildStripsHeaderInjection(t *testing.T) {
	m := Build("星枢 <noreply@example.com>", []string{"a@example.com"}, "重置密码\r\nBcc: evil@example.com", "正文", time.Unix(0, 0))
	if strings.Contains(m, "\r\nBcc:") {
		t.Fatal("header injection not stripped")
	}
	if !strings.Contains(m, "Subject: =?UTF-8?b?") {
		t.Fatal("subject must be B-encoded")
	}
}

func TestValidate(t *testing.T) {
	c := Config{Enabled: true, Host: "smtp.example.com", From: "noreply@example.com"}
	if err := c.Validate(); err != nil || c.Port != 587 {
		t.Fatalf("valid config rejected: %v", err)
	}
	if (&Config{Enabled: true, Host: "h", From: "bad"}).Validate() == nil {
		t.Fatal("bad from must be rejected")
	}
}
