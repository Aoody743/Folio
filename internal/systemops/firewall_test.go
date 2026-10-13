package systemops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func kernelJSONFixture() []byte {
	return []byte(`{"nftables":[{"metainfo":{"json_schema_version":1}},{"table":{"family":"inet","name":"powerlinux"}},{"chain":{"family":"inet","table":"powerlinux","name":"input","type":"filter","hook":"input","prio":10,"policy":"drop"}},{"rule":{"family":"inet","table":"powerlinux","chain":"input","expr":[{"match":{"op":"in","left":{"ct":{"key":"state"}},"right":{"set":["established","related"]}}},{"accept":null}]}},{"rule":{"family":"inet","table":"powerlinux","chain":"input","expr":[{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"lo"}},{"accept":null}]}},{"rule":{"family":"inet","table":"powerlinux","chain":"input","expr":[{"match":{"op":"==","left":{"payload":{"protocol":"tcp","field":"dport"}},"right":{"set":[22,80,443,9000]}}},{"accept":null}]}},{"rule":{"family":"inet","table":"powerlinux","chain":"input","comment":"powerlinux:web","expr":[{"match":{"op":"==","left":{"payload":{"protocol":"ip","field":"saddr"}},"right":{"prefix":{"addr":"192.0.2.0","len":24}}}},{"match":{"op":"==","left":{"payload":{"protocol":"tcp","field":"dport"}},"right":8080}},{"accept":null}]}}]}`)
}
func TestFirewallJSONRejectsMisleadingPortTextAndWrongConditions(t *testing.T) {
	config := FirewallConfig{DefaultPolicy: "drop", Rules: []FirewallRule{{ID: "web", Protocol: "tcp", Port: 8080, Source: "192.0.2.0/24", Action: "accept"}}}
	ports := []int{22, 80, 443, 9000}
	if err := verifyFirewallJSON(kernelJSONFixture(), config, ports); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"port", "source", "policy", "established"} {
		var document map[string]any
		if err := json.Unmarshal(kernelJSONFixture(), &document); err != nil {
			t.Fatal(err)
		}
		nodes := document["nftables"].([]any)
		switch kind {
		case "port":
			rule := asObject(asObject(nodes[5])["rule"])
			expr := rule["expr"].([]any)
			match := asObject(asObject(expr[0])["match"])
			asObject(match["right"])["set"] = []int{2222, 80, 443, 9000}
		case "source":
			rule := asObject(asObject(nodes[6])["rule"])
			expr := rule["expr"].([]any)
			match := asObject(asObject(expr[0])["match"])
			asObject(asObject(match["right"])["prefix"])["addr"] = "198.51.100.0"
		case "policy":
			asObject(asObject(nodes[2])["chain"])["policy"] = "accept"
		case "established":
			rule := asObject(asObject(nodes[3])["rule"])
			expr := rule["expr"].([]any)
			match := asObject(asObject(expr[0])["match"])
			asObject(match["right"])["set"] = []string{"invalid", "related"}
		}
		raw, _ := json.Marshal(document)
		if err := verifyFirewallJSON(raw, config, ports); err == nil {
			t.Fatal("unverified kernel state accepted", kind)
		}
	}
}

func TestActualDebianNftKernelJSON(t *testing.T) {
	raw, err := os.ReadFile("testdata/nft-1.1.3-debian13.json")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err = json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	// Normalize only the isolated fixture's namespace/comment labels into the
	// owned application's names. Kernel-generated conditions remain untouched.
	for _, node := range document["nftables"].([]any) {
		for kind, value := range asObject(node) {
			object := asObject(value)
			if kind == "table" {
				object["name"] = "powerlinux"
			}
			if _, ok := object["table"]; ok {
				object["table"] = "powerlinux"
			}
			if object["comment"] == "qa reserved ports" {
				delete(object, "comment")
			}
			if object["comment"] == "qa cidr8080" {
				object["comment"] = "powerlinux:web"
			}
		}
	}
	fixture, _ := json.Marshal(document)
	config := FirewallConfig{DefaultPolicy: "accept", Rules: []FirewallRule{{ID: "web", Protocol: "tcp", Port: 8080, Source: "192.0.2.0/24", Action: "accept"}}}
	if err = verifyFirewallJSON(fixture, config, []int{22, 80, 443, 9000}); err != nil {
		t.Fatal(err)
	}
}

// This fixture proves private receipt persistence and boot recovery behavior,
// independently of the actual kernel nftables VM gate.
func TestFirewallBootRestoresOnlyOwnedAbsentTable(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "fake-kernel-table")
	unrelated := filepath.Join(dir, "unrelated-table")
	if err := os.WriteFile(unrelated, []byte("preserve foreign table"), 0600); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "nft-fixture")
	text := fmt.Sprintf("#!/bin/bash\nset -eu\ncase \"$1\" in\nlist) if test -f '%s'; then cat '%s'; else printf 'No such file or directory' >&2; exit 1; fi ;;\n--check) exit 0 ;;\n--file) cat \"$2\" > '%s' ;;\n*) exit 29 ;;\nesac\n", kernel, kernel, kernel)
	if err := os.WriteFile(program, []byte(text), 0700); err != nil {
		t.Fatal(err)
	}
	config := HostConfig{Version: 1, StateDir: filepath.Join(dir, "state"), Programs: map[string]string{"nft": program}, EnabledKinds: []string{"firewall_rules"}}
	first, err := NewHostExecutor(config)
	if err != nil {
		t.Fatal(err)
	}
	wanted := first.firewallText(FirewallConfig{DefaultPolicy: "accept"}, false)
	if err = first.persistFirewall(wanted); err != nil {
		t.Fatal(err)
	}
	if err = writePrivate(filepath.Join(config.StateDir, "firewall-owned"), []byte("PowerLinux nftables ownership v1\n")); err != nil {
		t.Fatal(err)
	}
	first.Close()
	restarted, err := NewHostExecutor(config)
	if err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(kernel)
	if string(restored) != string(wanted) {
		t.Fatal("owned table was not restored")
	}
	restarted.Close()
	if err = os.WriteFile(kernel, []byte("operator-modified kernel table"), 0600); err != nil {
		t.Fatal(err)
	}
	again, err := NewHostExecutor(config)
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
	current, _ := os.ReadFile(kernel)
	foreign, _ := os.ReadFile(unrelated)
	if string(current) != "operator-modified kernel table" || string(foreign) != "preserve foreign table" {
		t.Fatal("boot recovery overwrote unrelated/current kernel state")
	}
}
