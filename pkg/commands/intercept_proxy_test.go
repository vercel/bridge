package commands

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ruleIndex(rules [][]string, contains ...string) int {
	return slices.IndexFunc(rules, func(rule []string) bool {
		joined := strings.Join(rule, " ")
		for _, s := range contains {
			if !strings.Contains(joined, s) {
				return false
			}
		}
		return true
	})
}

// The intercept's own lookups (the k8spf redial: API server, then aws eks
// get-token → STS) must reach the bypass DNS server, never the one that asks
// the tunnel they are trying to rebuild. /etc/resolv.conf names only
// 127.0.0.1, so letting the group skip the redirect isn't enough.
func TestIptablesRules_SendsBridgesOwnDNSToTheBypassServer(t *testing.T) {
	p := &ProxyComponent{port: 15001, dnsPort: 53, excludeGID: 999, bypassDNSPort: 40123}
	rules := p.iptablesRules()

	own := ruleIndex(rules, "--gid-owner 999", "-p udp --dport 53", "-j REDIRECT --to-ports 40123")
	require.NotEqual(t, -1, own, "bridge's group must be redirected to the bypass DNS server")
	assert.Equal(t, -1, ruleIndex(rules, "--gid-owner 999", "-j RETURN"),
		"RETURN leaves the query going to 127.0.0.1:53, which asks the broken tunnel")

	everyone := ruleIndex(rules, "BRIDGE_INTERCEPT -p udp --dport 53 -j REDIRECT --to-ports 53")
	require.NotEqual(t, -1, everyone)
	assert.Less(t, own, everyone, "bridge's rule must come first or the general redirect wins")
}

func TestIptablesRules_NoGroupRuleWithoutABypassServerOrGroup(t *testing.T) {
	for name, p := range map[string]*ProxyComponent{
		"root group":       {port: 15001, dnsPort: 53, excludeGID: 0, bypassDNSPort: 40123},
		"no bypass server": {port: 15001, dnsPort: 53, excludeGID: 999, bypassDNSPort: 0},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, -1, ruleIndex(p.iptablesRules(), "--gid-owner"))
		})
	}
}

func TestIptablesRules_NoDNSRulesWithoutDNS(t *testing.T) {
	p := &ProxyComponent{port: 15001, excludeGID: 999, bypassDNSPort: 40123}
	assert.Equal(t, -1, ruleIndex(p.iptablesRules(), "--dport 53"))
}
