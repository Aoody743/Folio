package sites

import (
	"context"
	"encoding/base64"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

type legacyKnotFixture struct {
	mu      sync.Mutex
	values  []string
	updates int
	race    bool
}

func legacyPrivateDNSState(t *testing.T) *os.Root {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

func legacyKnotProtocolFixture(t *testing.T) (legacyDNSOptions, *legacyKnotFixture) {
	t.Helper()
	secret := base64.StdEncoding.EncodeToString([]byte("32-byte-test-only-TSIG-key-abcdefg"))
	key := "powerlinux-fixture."
	state := &legacyKnotFixture{values: []string{"unrelated-existing-value"}, race: true}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{Listener: listener, Net: "tcp", TsigSecret: map[string]string{key: secret}}
	// miekg's default recursive-server header filter rejects RFC2136 UPDATE
	// before its handler/TSIG verification. This fixture is an authoritative
	// update server; allow its bounded update shape and retain TSIG checks.
	server.MsgAcceptFunc = func(header dns.Header) dns.MsgAcceptAction {
		opcode := int(header.Bits>>11) & 0xf
		if header.Bits&0x8000 != 0 {
			return dns.MsgIgnore
		}
		if opcode != dns.OpcodeQuery && opcode != dns.OpcodeUpdate {
			return dns.MsgRejectNotImplemented
		}
		if header.Qdcount != 1 || header.Ancount > 2000 || header.Nscount > 1 || header.Arcount != 1 {
			return dns.MsgReject
		}
		return dns.MsgAccept
	}
	server.Handler = dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		state.mu.Lock()
		defer state.mu.Unlock()
		response := new(dns.Msg).SetReply(request)
		response.Authoritative = true
		if request.IsTsig() == nil || writer.TsigStatus() != nil {
			response.Rcode = dns.RcodeNotAuth
			writer.WriteMsg(response)
			return
		}
		if request.Opcode == dns.OpcodeQuery && request.Question[0].Qtype == dns.TypeSOA {
			if request.Question[0].Name == "example.test." {
				response.Answer = []dns.RR{&dns.SOA{Hdr: dns.RR_Header{Name: "example.test.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 60}, Ns: "ns.example.test.", Mbox: "hostmaster.example.test.", Serial: 1, Refresh: 60, Retry: 60, Expire: 3600, Minttl: 60}}
			}
		} else if request.Opcode == dns.OpcodeQuery && request.Question[0].Qtype == dns.TypeTXT {
			for _, value := range state.values {
				response.Answer = append(response.Answer, legacyKnotTXT(request.Question[0].Name, value, 60))
			}
		} else if request.Opcode == dns.OpcodeUpdate {
			state.updates++
			if len(request.Question) != 1 || request.Question[0].Name != "example.test." || len(request.Ns) != 1 {
				t.Error("unexpected typed update shape")
				response.Rcode = dns.RcodeRefused
			} else {
				change, ok := request.Ns[0].(*dns.TXT)
				if !ok || len(change.Txt) != 1 || change.Hdr.Class == dns.ClassANY {
					t.Error("adapter attempted broad RRset deletion")
					response.Rcode = dns.RcodeRefused
				} else if change.Hdr.Class == dns.ClassINET {
					if state.race {
						state.race = false
						state.values = append(state.values, "external-concurrent-value")
						response.Rcode = dns.RcodeNXRrset
					} else {
						observed := map[string]bool{}
						for _, prerequisite := range request.Answer {
							if txt, ok := prerequisite.(*dns.TXT); ok && txt.Hdr.Class == dns.ClassINET && txt.Hdr.Ttl == 0 {
								observed[strings.Join(txt.Txt, "")] = true
							}
						}
						if len(observed) != len(state.values) {
							response.Rcode = dns.RcodeNXRrset
						}
						for _, value := range state.values {
							if !observed[value] {
								response.Rcode = dns.RcodeNXRrset
							}
						}
						if response.Rcode == dns.RcodeSuccess {
							state.values = append(state.values, change.Txt[0])
						}
					}
				} else if change.Hdr.Class == dns.ClassNONE && change.Hdr.Ttl == 0 {
					next := []string{}
					for _, value := range state.values {
						if value != change.Txt[0] {
							next = append(next, value)
						}
					}
					state.values = next
				} else {
					t.Error("unexpected update class")
					response.Rcode = dns.RcodeRefused
				}
			}
		}
		response.SetTsig(key, dns.HmacSHA256, 300, time.Now().Unix())
		if err := writer.WriteMsg(response); err != nil {
			t.Error(err)
		}
	})
	go server.ActivateAndServe()
	t.Cleanup(func() { server.Shutdown() })
	return legacyDNSOptions{
		Provider: "dns_knot", OwnerID: "certificate-one", State: legacyPrivateDNSState(t),
		Credentials:       map[string]string{"KNOT_SERVER": listener.Addr().String(), "KNOT_KEY": "hmac-sha256:" + key + " " + secret},
		AllowedDNSServers: []string{listener.Addr().String()},
	}, state
}

func TestLegacyKnotVerifiedTSIGConditionalTXTAndDurableCleanup(t *testing.T) {
	options, state := legacyKnotProtocolFixture(t)
	provider, err := newLegacyDNSProvider(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := provider.Present(ctx, "*.example.test", "token-one", "token-one.thumbprint"); err != nil {
		t.Fatal(err)
	}
	client := provider.(*legacyDNSClient)
	name, value, _ := client.challenge("example.test", "token-one", "token-one.thumbprint")
	receipt, err := client.loadReceipt(name, value)
	if err != nil || receipt.State != "created" {
		t.Fatal("confirmed ownership receipt was not persisted", err)
	}
	state.mu.Lock()
	values, updates := append([]string(nil), state.values...), state.updates
	state.mu.Unlock()
	if len(values) != 3 || updates != 2 {
		t.Fatal("conditional retry lost unrelated/concurrent records", values, updates)
	}
	if err := provider.Present(ctx, "*.example.test", "token-one", "token-one.thumbprint"); err != nil {
		t.Fatal("idempotent present failed", err)
	}
	// Simulate interruption between successful remote creation and durable
	// acknowledgement; a fresh provider recovers only the exact new record.
	receipt.State, receipt.RecordID = "prepared", ""
	if err := client.saveReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	restarted, err := newLegacyDNSProvider(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.CleanUp(ctx, "*.example.test", "token-one", "token-one.thumbprint"); err != nil {
		t.Fatal("durable cleanup recovery failed", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.values) != 2 || state.values[0] != "unrelated-existing-value" || state.values[1] != "external-concurrent-value" {
		t.Fatal("cleanup removed somebody else's TXT value", state.values)
	}
	directory, err := options.State.Open("acme/dns-receipts")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil || len(entries) != 0 {
		t.Fatal("verified cleanup left its durable receipt", err)
	}
}

func TestLegacyKnotBorrowedRecordsAndWrongTSIGRemainUntouched(t *testing.T) {
	options, state := legacyKnotProtocolFixture(t)
	provider, err := newLegacyDNSProvider(options)
	if err != nil {
		t.Fatal(err)
	}
	client := provider.(*legacyDNSClient)
	_, value, _ := client.challenge("example.test", "already-present", "already-present.thumbprint")
	state.mu.Lock()
	state.values = append(state.values, value)
	state.mu.Unlock()
	ctx := context.Background()
	if err := provider.Present(ctx, "example.test", "already-present", "already-present.thumbprint"); err != nil {
		t.Fatal(err)
	}
	if err := provider.CleanUp(ctx, "example.test", "already-present", "already-present.thumbprint"); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	remaining, updates := len(state.values), state.updates
	state.mu.Unlock()
	if remaining != 2 || updates != 0 {
		t.Fatal("pre-existing matching value was modified")
	}
	bad := options
	bad.Credentials = cloneLegacyDNSCredentials(options.Credentials)
	bad.Credentials["KNOT_KEY"] = "hmac-sha256:powerlinux-fixture. " + base64.StdEncoding.EncodeToString([]byte("a-different-32-byte-test-only-key"))
	unverified, err := newLegacyDNSProvider(bad)
	if err != nil {
		t.Fatal(err)
	}
	if err := unverified.Present(ctx, "example.test", "wrong-secret", "wrong-secret.thumbprint"); err == nil {
		t.Fatal("wrong TSIG was accepted")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.updates != 0 {
		t.Fatal("wrong TSIG produced an update")
	}
}
