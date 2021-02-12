// +build !confonly

package dns

import (
	"v2ray.com/core/common"
	"v2ray.com/core/common/net"
	"v2ray.com/core/common/strmatcher"
	"v2ray.com/core/features"
)

// StaticHosts represents static domain-ip mapping in DNS server.
type StaticHosts struct {
	ips      [][]net.Address
	matchers *strmatcher.MatcherGroup
}

// NewStaticHosts creates a new StaticHosts instance.
func NewStaticHosts(hosts []*Config_HostMapping, legacy map[string]*net.IPOrDomain) (*StaticHosts, error) {
	g := new(strmatcher.MatcherGroup)
	sh := &StaticHosts{
		ips:      make([][]net.Address, len(hosts)+len(legacy)+16),
		matchers: g,
	}

	if legacy != nil {
		features.PrintDeprecatedFeatureWarning("simple host mapping")

		for domain, ip := range legacy {
			matcher, err := strmatcher.Full.New(domain)
			common.Must(err)
			id := g.Add(matcher)

			address := ip.AsAddress()
			if address.Family().IsDomain() {
				return nil, newError("invalid domain address in static hosts: ", address.Domain()).AtWarning()
			}

			sh.ips[id] = []net.Address{address}
		}
	}

	for _, mapping := range hosts {
		matcher, err := toStrMatcher(mapping.Type, mapping.Domain)
		if err != nil {
			return nil, newError("failed to create domain matcher").Base(err)
		}
		id := g.Add(matcher)
		ips := make([]net.Address, 0, len(mapping.Ip)+1)
		switch {
		case len(mapping.Ip) > 0:
			for _, ip := range mapping.Ip {
				addr := net.IPAddress(ip)
				if addr == nil {
					return nil, newError("invalid IP address in static hosts: ", ip).AtWarning()
				}
				ips = append(ips, addr)
			}

		case len(mapping.ProxiedDomain) > 0:
			ips = append(ips, net.DomainAddress(mapping.ProxiedDomain))

		default:
			return nil, newError("neither IP address nor proxied domain specified for domain: ", mapping.Domain).AtWarning()
		}

		// Special handling for localhost IPv6. This is a dirty workaround as JSON config supports only single IP mapping.
		if len(ips) == 1 && ips[0] == net.LocalHostIP {
			ips = append(ips, net.LocalHostIPv6)
		}

		sh.ips[id] = ips
	}

	return sh, nil
}

func filterIP(ips []net.Address, option IPOption) []net.Address {
	filtered := make([]net.Address, 0, len(ips))
	for _, ip := range ips {
		if (ip.Family().IsIPv4() && option.IPv4Enable) || (ip.Family().IsIPv6() && option.IPv6Enable) {
			filtered = append(filtered, ip)
		}
	}
	return filtered
}

func (h *StaticHosts) lookupInternal(domain string) []net.Address {
	var ips []net.Address
	for _, id := range h.matchers.Match(domain) {
		ips = append(ips, h.ips[id]...)
	}
	return ips
}

func (h *StaticHosts) lookup(domain string, option IPOption, maxDepth int) []net.Address {
	switch addrs := h.lookupInternal(domain); {
	case len(addrs) == 0: // Not recorded in static hosts, return nil
		return nil
	case len(addrs) == 1 && addrs[0].Family().IsDomain(): // Try to unwrap domain
		if maxDepth > 0 {
			unwrapped := h.lookup(addrs[0].Domain(), option, maxDepth-1)
			if unwrapped != nil {
				return unwrapped
			}
		}
		return addrs
	default: // IP record found, return a non-nil IP array
		return filterIP(addrs, option)
	}
}

// Lookup returns IP addresses or proxied domain for the given domain, if exists in this StaticHosts.
func (h *StaticHosts) Lookup(domain string, option IPOption) []net.Address {
	return h.lookup(domain, option, 5)
}
