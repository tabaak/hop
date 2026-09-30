package dnsprovider

import (
	"strings"
	"testing"

	"github.com/libdns/cloudflare"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestNew(t *testing.T) {
	p, err := New("", envOf(nil))
	if p != nil || err != nil {
		t.Fatalf("empty name: got %v, %v; want nil, nil", p, err)
	}

	if _, err := New("nosuch", envOf(nil)); err == nil || !strings.Contains(err.Error(), "cloudflare") {
		t.Fatalf("unknown provider should list the supported ones, got %v", err)
	}

	// Every missing variable is named at once.
	_, err = New("porkbun", envOf(map[string]string{}))
	if err == nil || !strings.Contains(err.Error(), "PORKBUN_API_KEY") || !strings.Contains(err.Error(), "PORKBUN_SECRET_API_KEY") {
		t.Fatalf("want both porkbun variables named, got %v", err)
	}

	p, err = New(" Cloudflare ", envOf(map[string]string{"CLOUDFLARE_API_TOKEN": "tok"}))
	if err != nil {
		t.Fatal(err)
	}
	if cf, ok := p.(*cloudflare.Provider); !ok || cf.APIToken != "tok" {
		t.Fatalf("got %#v", p)
	}

	// route53 can take credentials from the instance role, so needs no env.
	if _, err := New("route53", envOf(nil)); err != nil {
		t.Fatalf("route53 without env: %v", err)
	}
}

func TestEveryProviderBuilds(t *testing.T) {
	for _, n := range Names() {
		env := map[string]string{}
		for _, k := range specs[n].required {
			env[k] = "x"
		}
		if p, err := New(n, envOf(env)); err != nil || p == nil {
			t.Errorf("%s: %v", n, err)
		}
	}
}

func TestDetect(t *testing.T) {
	if got := Detect(envOf(map[string]string{"CLOUDFLARE_API_TOKEN": "t"})); got != "cloudflare" {
		t.Errorf("got %q", got)
	}
	if got := Detect(envOf(map[string]string{"AWS_ACCESS_KEY_ID": "k"})); got != "" {
		t.Errorf("AWS variables alone should not select route53, got %q", got)
	}
}
