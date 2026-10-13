package systemops

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
)

type firewallReceipt struct {
	Value string `json:"value"`
	Hash  string `json:"hash"`
}

func (e *HostExecutor) persistFirewall(text []byte) error {
	value, _ := json.Marshal(firewallReceipt{Value: string(text), Hash: hashBytes(text)})
	return writePrivate(filepath.Join(e.config.StateDir, "firewall-current.json"), value)
}
func (e *HostExecutor) restoreFirewallBoot(ctx context.Context) error {
	if !e.kinds["firewall_rules"] {
		return nil
	}
	path := filepath.Join(e.config.StateDir, "firewall-current.json")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	config, err := readPrivateJSON(path, 2<<20)
	if err != nil {
		return err
	}
	var receipt firewallReceipt
	if err = json.Unmarshal(config, &receipt); err != nil {
		return err
	}
	if receipt.Hash != hashBytes([]byte(receipt.Value)) || !strings.HasPrefix(strings.TrimSpace(receipt.Value), "table inet powerlinux {") {
		return errors.New("invalid owned firewall receipt")
	}
	if _, err = os.Stat(filepath.Join(e.config.StateDir, "firewall-owned")); err != nil {
		return errors.New("firewall receipt lacks ownership marker")
	}
	current, err := e.command(ctx, "nft", "list", "table", "inet", "powerlinux")
	if err == nil {
		return nil
	} // Existing kernel state may have operator changes.
	if !strings.Contains(current.Stderr, "No such file or directory") {
		return err
	}
	stage, err := e.stage([]byte(receipt.Value))
	if err != nil {
		return err
	}
	defer os.Remove(stage)
	if _, err = e.command(ctx, "nft", "--check", "--file", stage); err != nil {
		return err
	}
	_, err = e.command(ctx, "nft", "--file", stage)
	return err
}

func (e *HostExecutor) verifyFirewall(ctx context.Context, config FirewallConfig) error {
	result, err := e.command(ctx, "nft", "--json", "list", "table", "inet", "powerlinux")
	if err != nil {
		return err
	}
	if result.Truncated {
		return errors.New("owned firewall JSON exceeded verification limit")
	}
	return verifyFirewallJSON([]byte(result.Stdout), config, e.config.ProtectedPorts)
}
func verifyFirewallJSON(raw []byte, config FirewallConfig, ports []int) error {
	var document struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if json.Unmarshal(raw, &document) != nil {
		return errors.New("invalid kernel firewall JSON")
	}
	table, chain, management, established, loopback := false, false, false, false, false
	rules := map[string]map[string]any{}
	ruleCount := 0
	for _, node := range document.Nftables {
		for kind, value := range node {
			if kind == "metainfo" {
				continue
			}
			var object map[string]any
			if json.Unmarshal(value, &object) != nil || object["family"] != "inet" {
				return errors.New("unexpected firewall object")
			}
			switch kind {
			case "table":
				table = object["name"] == "powerlinux"
			case "chain":
				chain = object["table"] == "powerlinux" && object["name"] == "input" && object["type"] == "filter" && object["hook"] == "input" && object["prio"] == float64(10) && object["policy"] == config.DefaultPolicy
			case "rule":
				if object["table"] != "powerlinux" || object["chain"] != "input" {
					return errors.New("unexpected firewall chain rule")
				}
				ruleCount++
				expr, ok := object["expr"].([]any)
				if !ok {
					return errors.New("invalid firewall expression")
				}
				comment, _ := object["comment"].(string)
				if comment == "" && len(expr) == 2 && hasVerdict(expr, "accept") {
					match, _ := asObject(expr[0])["match"].(map[string]any)
					left := asObject(match["left"])
					payload := asObject(left["payload"])
					if payload["protocol"] == "tcp" && payload["field"] == "dport" && (match["op"] == "==" || match["op"] == "in") && samePorts(match["right"], ports) {
						management = true
					}
					meta := asObject(left["meta"])
					if meta["key"] == "iifname" && match["op"] == "==" && match["right"] == "lo" {
						loopback = true
					}
					ct := asObject(left["ct"])
					if ct["key"] == "state" && (match["op"] == "in" || match["op"] == "==") && sameStrings(match["right"], []string{"established", "related"}) {
						established = true
					}
				}
				if strings.HasPrefix(comment, "powerlinux:") {
					rules[strings.TrimPrefix(comment, "powerlinux:")] = object
				}
			default:
				return errors.New("unexpected firewall node")
			}
		}
	}
	if !table || !chain || !management || !established || !loopback || ruleCount != 3+len(config.Rules) || len(rules) != len(config.Rules) {
		return errors.New("firewall policy or management protection did not match")
	}
	for _, rule := range config.Rules {
		object, ok := rules[rule.ID]
		if !ok {
			return errors.New("requested firewall rule missing")
		}
		expr, _ := object["expr"].([]any)
		if !hasVerdict(expr, rule.Action) {
			return errors.New("requested firewall verdict did not match")
		}
		conditions := 0
		if rule.Source != "" {
			conditions++
		}
		if rule.Protocol != "any" {
			conditions++
		}
		if len(expr) != conditions+1 {
			return errors.New("requested firewall expression count did not match")
		}
		if rule.Port != 0 {
			found := false
			for _, expression := range expr {
				match := asObject(asObject(expression)["match"])
				payload := asObject(asObject(match["left"])["payload"])
				if payload["protocol"] == rule.Protocol && payload["field"] == "dport" && match["op"] == "==" && match["right"] == float64(rule.Port) {
					found = true
				}
			}
			if !found {
				return errors.New("requested firewall port did not match")
			}
		}
		if rule.Source != "" {
			wanted, _ := netip.ParsePrefix(rule.Source)
			family := "ip"
			if wanted.Addr().Is6() {
				family = "ip6"
			}
			found := false
			for _, expression := range expr {
				match := asObject(asObject(expression)["match"])
				payload := asObject(asObject(match["left"])["payload"])
				prefix := asObject(asObject(match["right"])["prefix"])
				if payload["protocol"] == family && payload["field"] == "saddr" && match["op"] == "==" && prefix["addr"] == wanted.Masked().Addr().String() && prefix["len"] == float64(wanted.Bits()) {
					found = true
				}
			}
			if !found {
				return errors.New("requested firewall source did not match")
			}
		}
		if rule.Protocol != "any" && rule.Port == 0 {
			found := false
			protocol := float64(6)
			if rule.Protocol == "udp" {
				protocol = 17
			}
			for _, expression := range expr {
				match := asObject(asObject(expression)["match"])
				meta := asObject(asObject(match["left"])["meta"])
				if meta["key"] == "l4proto" && match["op"] == "==" && (match["right"] == rule.Protocol || match["right"] == protocol) {
					found = true
				}
			}
			if !found {
				return errors.New("requested firewall protocol did not match")
			}
		}
	}
	return nil
}
func asObject(value any) map[string]any { out, _ := value.(map[string]any); return out }
func hasVerdict(expressions []any, verdict string) bool {
	for _, expression := range expressions {
		if _, ok := asObject(expression)[verdict]; ok {
			return true
		}
	}
	return false
}
func samePorts(value any, wanted []int) bool {
	actual := []int{}
	if number, ok := value.(float64); ok {
		actual = append(actual, int(number))
	} else {
		set, ok := asObject(value)["set"].([]any)
		if !ok {
			return false
		}
		for _, item := range set {
			number, ok := item.(float64)
			if !ok || number != float64(int(number)) {
				return false
			}
			actual = append(actual, int(number))
		}
	}
	if len(actual) != len(wanted) {
		return false
	}
	found := map[int]bool{}
	for _, number := range actual {
		found[number] = true
	}
	for _, number := range wanted {
		if !found[number] {
			return false
		}
	}
	return true
}
func sameStrings(value any, wanted []string) bool {
	set, ok := value.([]any)
	if !ok {
		set, ok = asObject(value)["set"].([]any)
	}
	if !ok || len(set) != len(wanted) {
		return false
	}
	found := map[string]bool{}
	for _, item := range set {
		text, ok := item.(string)
		if !ok {
			return false
		}
		found[text] = true
	}
	for _, text := range wanted {
		if !found[text] {
			return false
		}
	}
	return true
}
