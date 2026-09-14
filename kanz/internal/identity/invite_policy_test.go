package identity

import "testing"

func TestInviteDomainPolicy(t *testing.T) {
	p, err := ParseInviteDomainPolicy(" Eighred.co, gmail.com ")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"user:akifgrape@eighred.co", "akifgrape@gmail.com", "user:Akif@EIGHRED.CO"} {
		if err := p.Check(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{"user:akif", "a@evil-eighred.co", "a@sub.eighred.co", "a@eighred.co.evil.com", "Akif <a@eighred.co>", "a@eighred.co.", "a@eighred.co\r\nBcc: b@gmail.com", "operator:a@eighred.co"} {
		if err := p.Check(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	for _, raw := range []string{"*", "*.eighred.co", "com", ",", "eighred.co,", "a@eighred.co", "-bad.co", "bad-.co", "bad..co", "https://eighred.co"} {
		if _, err := ParseInviteDomainPolicy(raw); err == nil {
			t.Errorf("accepted configuration %q", raw)
		}
	}
	if err := (InviteDomainPolicy{}).Check("user:legacy"); err != nil {
		t.Fatal(err)
	}
}
