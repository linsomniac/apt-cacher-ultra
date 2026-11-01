// Package dnsfixture provides a local authoritative DNS server with authenticated
// RFC 2136 updates for real Lego provider and Pebble integration tests.
package dnsfixture

import (
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

type Server struct {
	mu          sync.Mutex
	records     map[string][]string
	aliases     map[string]string
	Address     string
	Key, Secret string
	Updates     int
}

func New(t testing.TB) *Server {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		_ = tcp.Close()
		t.Fatal(err)
	}
	s := &Server{Address: tcp.Addr().String(), records: make(map[string][]string), aliases: make(map[string]string), Key: "test.example.net.", Secret: base64.StdEncoding.EncodeToString([]byte("local-fixture-key-with-no-external-use"))}
	handler := dns.HandlerFunc(s.serve)
	tsig := map[string]string{s.Key: s.Secret}
	servers := []*dns.Server{{Listener: tcp, Handler: handler, TsigSecret: tsig}, {PacketConn: udp, Handler: handler, TsigSecret: tsig}}
	var wg sync.WaitGroup
	for _, srv := range servers {
		srv.MsgAcceptFunc = func(dns.Header) dns.MsgAcceptAction { return dns.MsgAccept }
		ready := make(chan struct{})
		srv.NotifyStartedFunc = func() { close(ready) }
		wg.Add(1)
		go func() { defer wg.Done(); _ = srv.ActivateAndServe() }()
		<-ready
	}
	t.Cleanup(func() {
		for _, srv := range servers {
			_ = srv.Shutdown()
		}
		wg.Wait()
	})
	return s
}

func (s *Server) ConfigureLego(t *testing.T) {
	t.Helper()
	// Never use ambient production credentials or alternative TSIG sources.
	for _, variable := range os.Environ() {
		key, _, _ := strings.Cut(variable, "=")
		if strings.HasPrefix(key, "DNSUPDATE_") || strings.HasPrefix(key, "RFC2136_") {
			t.Setenv(key, "")
		}
	}
	t.Setenv("LEGO_DISABLE_CNAME_SUPPORT", "false")
	t.Setenv("DNSUPDATE_NAMESERVER", s.Address)
	t.Setenv("DNSUPDATE_ZONES", "example.net.")
	t.Setenv("DNSUPDATE_TSIG_KEY", s.Key)
	t.Setenv("DNSUPDATE_TSIG_ALGORITHM", "hmac-sha256.")
	path := filepath.Join(t.TempDir(), "tsig-secret")
	if err := os.WriteFile(path, []byte(s.Secret), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DNSUPDATE_TSIG_SECRET", "")
	t.Setenv("DNSUPDATE_TSIG_SECRET_FILE", path)
	t.Setenv("DNSUPDATE_PROPAGATION_TIMEOUT", "5")
	t.Setenv("DNSUPDATE_POLLING_INTERVAL", "1")
}

func (s *Server) Add(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[dns.Fqdn(name)] = append(s.records[dns.Fqdn(name)], value)
}
func (s *Server) Remove(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = dns.Fqdn(name)
	out := s.records[name][:0]
	for _, v := range s.records[name] {
		if v != value {
			out = append(out, v)
		}
	}
	s.records[name] = out
}
func (s *Server) Alias(name, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aliases[dns.Fqdn(name)] = dns.Fqdn(target)
}
func (s *Server) Records(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.records[dns.Fqdn(name)]...)
}
func (s *Server) UpdateCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.Updates }

func (s *Server) serve(w dns.ResponseWriter, r *dns.Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	if r.Opcode == dns.OpcodeUpdate {
		if r.IsTsig() == nil || w.TsigStatus() != nil {
			m.Rcode = dns.RcodeRefused
			_ = w.WriteMsg(m)
			return
		}
		s.Updates++
		for _, rr := range r.Ns {
			txt, ok := rr.(*dns.TXT)
			if !ok {
				m.Rcode = dns.RcodeRefused
				continue
			}
			name := txt.Hdr.Name
			value := strings.Join(txt.Txt, "")
			switch txt.Hdr.Class {
			case dns.ClassINET:
				s.records[name] = append(s.records[name], value)
			case dns.ClassANY:
				delete(s.records, name)
			case dns.ClassNONE:
				out := s.records[name][:0]
				for _, v := range s.records[name] {
					if v != value {
						out = append(out, v)
					}
				}
				s.records[name] = out
			default:
				m.Rcode = dns.RcodeRefused
			}
		}
		sig := r.IsTsig()
		m.SetTsig(sig.Hdr.Name, sig.Algorithm, 300, time.Now().Unix())
	} else if len(r.Question) == 1 {
		q := r.Question[0]
		name := q.Name
		if target, ok := s.aliases[name]; ok {
			m.Answer = append(m.Answer, &dns.CNAME{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 1}, Target: target})
			name = target
		}
		header := dns.RR_Header{Name: name, Rrtype: q.Qtype, Class: dns.ClassINET, Ttl: 1}
		switch q.Qtype {
		case dns.TypeSOA:
			m.Answer = append(m.Answer, &dns.SOA{Hdr: dns.RR_Header{Name: "example.net.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 1}, Ns: "ns.example.net.", Mbox: "hostmaster.example.net.", Serial: 1, Refresh: 60, Retry: 60, Expire: 3600, Minttl: 1})
		case dns.TypeNS:
			m.Answer = append(m.Answer, &dns.NS{Hdr: header, Ns: "ns.example.net."})
		case dns.TypeA:
			m.Answer = append(m.Answer, &dns.A{Hdr: header, A: net.ParseIP("127.0.0.1")})
		case dns.TypeTXT:
			for _, value := range s.records[name] {
				m.Answer = append(m.Answer, &dns.TXT{Hdr: header, Txt: []string{value}})
			}
		}
	}
	_ = w.WriteMsg(m)
}
