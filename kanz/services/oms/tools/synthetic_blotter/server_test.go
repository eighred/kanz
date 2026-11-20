package syntheticblotter

import (
	"net/url"
	"testing"
)

// These are inert URL fields. The cases exercise only destination selection.
func TestReadSourceSettings(t *testing.T) {
	for _, key := range []string{"PGSERVICE", "PGSERVICEFILE", "PGHOSTADDR", "PGOPTIONS"} {
		t.Setenv(key, "")
	}
	base := url.URL{Scheme: "postgres", User: url.User(readRole), Host: "127.0.0.1:5432", Path: "/" + readDatabase, RawQuery: "sslmode=disable"}
	if !allowReadSource(&base) || allowReadSource(nil) {
		t.Fatal("explicit synthetic reader setting required")
	}
	for _, tc := range []struct {
		host    string
		allowed bool
	}{
		{"[::1]:15432", true}, {"127.0.0.1:15432", true},
		{"localhost:5432", false}, {"db.invalid:5432", false}, {"127.0.0.2:5432", false},
		{"127.0.0.1,db.invalid:5432", false}, {"db.invalid:5432,127.0.0.1:5432", false},
		{"127.0.0.1", false}, {"127.0.0.1:0", false}, {"127.0.0.1:65536", false},
	} {
		u := base
		u.Host = tc.host
		if allowReadSource(&u) != tc.allowed {
			t.Errorf("wrong classification for host form %q", tc.host)
		}
	}
	for _, mutate := range []func(*url.URL){
		func(u *url.URL) { u.User = nil },
		func(u *url.URL) { u.User = url.User("another-role") },
		func(u *url.URL) { u.Path = "/another_database" },
		func(u *url.URL) { u.Scheme = "https" },
		func(u *url.URL) { u.Fragment = "unexpected" },
	} {
		u := base
		mutate(&u)
		if allowReadSource(&u) {
			t.Fatal("accepted alternate database, role or protocol")
		}
	}
	for _, key := range []string{"PGSERVICE", "PGSERVICEFILE", "PGHOSTADDR", "PGOPTIONS"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "unexpected")
			if allowReadSource(&base) {
				t.Error("accepted inherited connection override")
			}
		})
	}
	for _, key := range []string{"hostaddr", "host", "port", "service", "servicefile", "options", "dbname", "user", "sslmode"} {
		u := base
		q := u.Query()
		q.Add(key, "unexpected")
		u.RawQuery = q.Encode()
		if allowReadSource(&u) {
			t.Errorf("accepted query override %q", key)
		}
	}
}
