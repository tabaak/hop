package server_test

import (
	"net"
	"strings"
	"testing"
	"time"

	"hop.vokh.dev/internal/client"
	"hop.vokh.dev/internal/proto"
)

// rawHello sends one hand-built opening frame and returns the server's reply,
// for the agents the real client can't be made to impersonate: older ones, and
// ones from a protocol that doesn't exist yet.
func rawHello(t *testing.T, h *harness, hello proto.Hello) proto.HelloAck {
	t.Helper()
	conn, err := net.Dial("tcp", h.agent.Server)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := proto.Write(conn, hello); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	var ack proto.HelloAck
	if err := proto.Read(conn, &ack); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	return ack
}

// An agent tells its user when the server is behind it, which it can only do
// if the server says what it is.
func TestAckReportsServerVersions(t *testing.T) {
	h := newHarness(t, false)

	var got proto.ServerInfo
	cfg := h.agent
	cfg.Subdomain = "reporter"
	cfg.OnServer = func(info proto.ServerInfo) { got = info }
	done := make(chan struct{})
	cfg.OnUp = func(string, string) { close(done) }
	go client.Run(cfg)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("second tunnel did not come up")
	}

	want := proto.ServerInfo{Release: testRelease, Protocol: proto.Version}
	if got != want {
		t.Errorf("server info = %+v, want %+v", got, want)
	}
}

// `hop version` asks through the listing, so the listing has to carry it too.
func TestListReportsServerVersions(t *testing.T) {
	h := newHarness(t, false)

	_, info, err := client.List(h.agent)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := proto.ServerInfo{Release: testRelease, Protocol: proto.Version}
	if info != want {
		t.Errorf("server info = %+v, want %+v", info, want)
	}
}

// Which release a server runs is only for someone who may use it.
func TestRefusedTokenLearnsNoVersion(t *testing.T) {
	h := newHarness(t, false)

	ack := rawHello(t, h, proto.Hello{Token: "not-the-token", Version: proto.Version, Subdomain: "x"})
	if ack.Code != proto.CodeBadToken {
		t.Fatalf("code = %q, want %q", ack.Code, proto.CodeBadToken)
	}
	if ack.ServerInfo != (proto.ServerInfo{}) {
		t.Errorf("a refused token was told %+v", ack.ServerInfo)
	}
}

// Every agent released so far sends protocol 1 and no release. A server that
// turned them away would break every installed hop on upgrade.
func TestAgentsFromBeforeVersioningStillConnect(t *testing.T) {
	h := newHarness(t, false)

	ack := rawHello(t, h, proto.Hello{Token: testToken, Version: "1", Subdomain: "oldagent", Local: h.local})
	if ack.Err != "" {
		t.Fatalf("pre-1.1 agent refused: %s (code %q)", ack.Err, ack.Code)
	}
}

// A protocol the server can't speak is refused with a sentence that names the
// side to upgrade, since agents that predate CodeVersion print it and nothing
// else.
func TestUnsupportedProtocolIsRefused(t *testing.T) {
	cases := []struct {
		name, version, wantSaid string
	}{
		{"agent too new", "99", "ask whoever runs hopd to upgrade it"},
		{"agent too old", "0", "upgrade hop"},
		{"not a number", "banana", "upgrade hop"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, false)

			ack := rawHello(t, h, proto.Hello{Token: testToken, Version: c.version, Subdomain: "future"})
			if ack.Code != proto.CodeVersion {
				t.Fatalf("code = %q, want %q (err %q)", ack.Code, proto.CodeVersion, ack.Err)
			}
			if !strings.Contains(ack.Err, c.wantSaid) {
				t.Errorf("err = %q, want it to say %q", ack.Err, c.wantSaid)
			}
			if ack.URL != "" || ack.Subdomain != "" {
				t.Errorf("a refused agent was handed a tunnel: %+v", ack)
			}
			// Nothing was claimed, so the name is free for someone who can use it.
			live, _, err := client.List(h.agent)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			for _, tun := range live {
				if tun.Subdomain == "future" {
					t.Errorf("refused agent still holds %q", tun.Subdomain)
				}
			}
		})
	}
}
