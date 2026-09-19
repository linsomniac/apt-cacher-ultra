package upstreamproxy

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for _, raw := range []string{
		"http://proxy.example", "https://proxy.example/",
		"http://proxy.example:1", "https://proxy.example:65535/",
		"http://127.0.0.1:3128", "http://[::1]", "https://[2001:db8::1]:3129/",
		"http://[fe80::1%25eth0]:3128",
		"http://user@proxy.example", "http://user:@proxy.example",
		"https://cache%40user:p%40ss%3Aword@proxy.example:3129",
	} {
		t.Run(raw, func(t *testing.T) {
			u, err := Parse(raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			if u == nil || u.String() != raw {
				t.Fatalf("Parse = %v, want unchanged valid URL", u)
			}
		})
	}
	u, err := Parse("https://cache%40user:p%40ss%3Aword@proxy.example:3129", nil)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	if u.User.Username() != "cache@user" || password != "p@ss:word" {
		t.Fatal("credentials were not decoded correctly")
	}
}

func TestParseRejectsInvalidProxyWithoutEchoingInput(t *testing.T) {
	for _, raw := range []string{
		" ", "http://proxy.example\n", "http://proxy.\x00example",
		"proxy.example:3128", "//proxy.example:3128", "http:///proxy.example",
		"http:proxy.example", "socks5://proxy.example:1080", "ftp://proxy.example",
		"http://", "http://:3128", "http://proxy.example:",
		"http://proxy.example:0", "http://proxy.example:65536",
		"http://proxy.example:99999999999999999999999", "http://proxy.example:-1",
		"http://proxy.example:abc", "http://proxy.example:80:90",
		"http://::1", "http://2001:db8::1", "http://[proxy.example]:3128",
		"http://[127.0.0.1]:3128", "http://[::1", "http://proxy.example]/",
		"http://proxy.example/path", "http://proxy.example/%2F",
		"http://proxy.example?", "http://proxy.example?token=secret",
		"http://proxy.example#", "http://proxy.example#secret",
		"http://@proxy.example", "http://:secret@proxy.example",
		"http://cache%3Auser:secret@proxy.example", "http://cache%0Auser:secret@proxy.example",
		"http://cache-user:secret%0D@proxy.example", "http://cache-user:secret%7F@proxy.example",
		"http://cache-user:secret%zz@proxy.example",
	} {
		t.Run(raw, func(t *testing.T) {
			u, err := Parse(raw, nil)
			if err == nil || u != nil {
				t.Fatal("invalid proxy accepted")
			}
			if !strings.Contains(err.Error(), "upstream.proxy") {
				t.Errorf("error does not identify setting: %v", err)
			}
			for _, sensitive := range []string{"cache-user", "secret", "proxy.example"} {
				if strings.Contains(err.Error(), sensitive) {
					t.Errorf("validation error disclosed input: %v", err)
				}
			}
		})
	}
}

func TestParseDirectAndDenyPolicy(t *testing.T) {
	deny := []string{"127.0.0.0/8"}
	for _, ranges := range [][]string{nil, {}, deny} {
		u, err := Parse("", ranges)
		if err != nil || u != nil {
			t.Fatalf("direct access = %v, %v; want nil URL without error", u, err)
		}
	}
	if _, err := Parse("http://proxy.example", deny); err == nil || !strings.Contains(err.Error(), "deny_target_ranges") {
		t.Fatalf("proxy with deny ranges: %v", err)
	}
}
