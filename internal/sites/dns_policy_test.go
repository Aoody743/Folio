package sites

import "testing"

func TestNativeDNSPolicyRequiresExactEndpointAndExclusiveChallengeSlot(t *testing.T) {
	valid := AgentConfig{AllowedDNSUpdateServers: map[string][]string{"dns_knot": {"192.0.2.1:53", "[2001:db8::1]:5353"}}, AllowedDNSExclusiveTXTSlots: map[string][]string{"dns_ddnss": {"_acme-challenge.example.ddnss.de"}}}
	if err := validateNativeDNSPolicy(valid); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"example.net:53", "0.0.0.0:53", "[::]:53", "224.0.0.1:53", "192.0.2.1:0", "192.0.2.1:53/path"} {
		t.Run(endpoint, func(t *testing.T) {
			if err := validateNativeDNSPolicy(AgentConfig{AllowedDNSUpdateServers: map[string][]string{"dns_knot": {endpoint}}}); err == nil {
				t.Fatal("nonliteral or unsafe endpoint accepted")
			}
		})
	}
	for _, name := range []string{"example.net", "_acme-challenge.*.example.net", "_acme-challenge.example.net.", "_acme-challenge.EXAMPLE.net", "_acme-challenge.example.net:443", "_acme-challenge.example.net; include"} {
		t.Run(name, func(t *testing.T) {
			if err := validateNativeDNSPolicy(AgentConfig{AllowedDNSExclusiveTXTSlots: map[string][]string{"dns_ddnss": {name}}}); err == nil {
				t.Fatal("broad or noncanonical exclusive slot accepted")
			}
		})
	}
	if err := validateNativeDNSPolicy(AgentConfig{AllowedDNSUpdateServers: map[string][]string{"dns_other": {"192.0.2.1:53"}}}); err == nil {
		t.Fatal("other provider inherited Knot transport authority")
	}
}
