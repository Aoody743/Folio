package files

import (
	"net"
	"testing"
)

func TestDownloadAddressRejectsSpecialUseNetworks(t *testing.T) {
	blocked := []string{
		"0.1.2.3", "10.1.2.3", "100.64.0.1", "100.100.100.200", "100.127.255.255",
		"127.0.0.1", "169.254.169.254", "172.16.0.1", "192.0.0.9", "192.0.2.1",
		"192.88.99.1", "192.168.1.1", "198.18.0.1", "198.19.255.255", "198.51.100.1",
		"203.0.113.1", "224.0.0.1", "240.0.0.1", "255.255.255.255", "::", "::1",
		"::ffff:100.100.100.200", "::ffff:192.0.2.1", "64:ff9b::6464:64c8", "64:ff9b:1::1",
		"100::1", "2001:2::1", "2001:db8::1", "2001:20::1", "2002:6464:64c8::1",
		"3fff::1", "fc00::1", "fe80::1", "fec0::1", "ff02::1",
	}
	for _, address := range blocked {
		t.Run(address, func(t *testing.T) {
			if publicIP(net.ParseIP(address)) {
				t.Fatalf("special-use address %s accepted as public", address)
			}
		})
	}
	for _, address := range []string{"1.1.1.1", "8.8.8.8", "100.63.255.255", "100.128.0.1", "2606:4700:4700::1111", "2001:4860:4860::8888", "::ffff:8.8.8.8"} {
		if !publicIP(net.ParseIP(address)) {
			t.Fatalf("public address %s rejected", address)
		}
	}
	if publicIP(nil) {
		t.Fatal("invalid address accepted")
	}
}
