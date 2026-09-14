package identity

import (
	"fmt"
	"net/mail"
	"strings"
)

// InviteDomainPolicy restricts new invitations to exact email domains. Its zero
// value allows existing non-email subjects. It does not establish email ownership.
type InviteDomainPolicy struct{ domains map[string]struct{} }

// ParseInviteDomainPolicy accepts a comma-separated list of DNS domains. Invalid
// entries fail closed; wildcards and suffix matching are deliberately unsupported.
func ParseInviteDomainPolicy(raw string) (InviteDomainPolicy, error) {
	p := InviteDomainPolicy{}
	if strings.TrimSpace(raw) == "" {
		return p, nil
	}
	p.domains = make(map[string]struct{})
	for _, value := range strings.Split(raw, ",") {
		d := strings.ToLower(strings.TrimSpace(value))
		if !validInviteDomain(d) {
			return InviteDomainPolicy{}, fmt.Errorf("invalid invitation email domain %q", value)
		}
		p.domains[d] = struct{}{}
	}
	return p, nil
}

func validInviteDomain(d string) bool {
	if len(d) > 253 || !strings.Contains(d, ".") {
		return false
	}
	for _, label := range strings.Split(d, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// Check accepts a bare email or a user:<email> subject when restricted.
func (p InviteDomainPolicy) Check(subject string) error {
	if len(p.domains) == 0 {
		return nil
	}
	s := strings.TrimPrefix(strings.TrimSpace(subject), "user:")
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || a.Name != "" {
		return fmt.Errorf("invitation requires an email address in an allowed domain")
	}
	at := strings.LastIndexByte(s, '@')
	if at <= 0 {
		return fmt.Errorf("invitation requires an email address in an allowed domain")
	}
	if _, ok := p.domains[strings.ToLower(s[at+1:])]; !ok {
		return fmt.Errorf("invitation email domain is not allowed")
	}
	return nil
}
