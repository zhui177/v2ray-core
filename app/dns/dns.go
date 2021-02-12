// +build !confonly

// Package dns is an implementation of core.DNS feature.
package dns

//go:generate go run v2ray.com/core/common/errors/errorgen

import (
	"context"
	"fmt"
	"sync"

	"v2ray.com/core/app/router"
	"v2ray.com/core/common"
	"v2ray.com/core/common/errors"
	"v2ray.com/core/common/net"
	"v2ray.com/core/common/session"
	"v2ray.com/core/common/strmatcher"
	"v2ray.com/core/features"
	"v2ray.com/core/features/dns"
)

// DNS is a DNS rely server.
type DNS struct {
	sync.Mutex
	tag           string
	hosts         *StaticHosts
	clients       []*Client
	ctx           context.Context
	domainMatcher strmatcher.IndexMatcher
	matcherInfos  []DomainMatcherInfo
}

// DomainMatcherInfo contains information attached to index returned by Server.domainMatcher
type DomainMatcherInfo struct {
	clientIdx     uint16
	domainRuleIdx uint16
}

// New creates a new DNS server with given configuration.
func New(ctx context.Context, config *Config) (*DNS, error) {
	var tag string
	if len(config.Tag) > 0 {
		tag = config.Tag
	} else {
		tag = generateRandomTag()
	}

	var clientIP net.IP
	switch len(config.ClientIp) {
	case 0, net.IPv4len, net.IPv6len:
		clientIP = net.IP(config.ClientIp)
	default:
		return nil, newError("unexpected client IP length ", len(config.ClientIp))
	}

	hosts, err := NewStaticHosts(config.StaticHosts, config.Hosts)
	if err != nil {
		return nil, newError("failed to create hosts").Base(err)
	}

	clients := []*Client{}
	domainRuleCount := 0
	for _, ns := range config.NameServer {
		domainRuleCount += len(ns.PrioritizedDomain)
	}
	// Fixes https://github.com/v2fly/v2ray-core/issues/529
	// Compatible with `localhost` nameserver specified in config file
	domainRuleCount += len(localTLDsAndDotlessDomains)

	// MatcherInfos is ensured to cover the maximum index domainMatcher could return, where matcher's index starts from 1
	matcherInfos := make([]DomainMatcherInfo, domainRuleCount+1)
	domainMatcher := &strmatcher.MatcherGroup{}
	geoipContainer := router.GeoIPMatcherContainer{}

	for _, endpoint := range config.NameServers {
		features.PrintDeprecatedFeatureWarning("simple DNS server")
		client, err := NewSimpleClient(ctx, endpoint, clientIP)
		if err != nil {
			return nil, newError("failed to create client").Base(err)
		}
		clients = append(clients, client)
	}

	for _, ns := range config.NameServer {
		clientIdx := len(clients)
		updateDomain := func(domainRule strmatcher.Matcher, originalRuleIdx int) error {
			midx := domainMatcher.Add(domainRule)
			matcherInfos[midx] = DomainMatcherInfo{
				clientIdx:     uint16(clientIdx),
				domainRuleIdx: uint16(originalRuleIdx),
			}
			return nil
		}

		myClientIP := clientIP
		switch len(ns.ClientIp) {
		case net.IPv4len, net.IPv6len:
			myClientIP = net.IP(ns.ClientIp)
		}
		client, err := NewClient(ctx, ns, myClientIP, geoipContainer, updateDomain)
		if err != nil {
			return nil, newError("failed to create client").Base(err)
		}
		clients = append(clients, client)
	}

	if len(clients) == 0 {
		clients = append(clients, NewLocalDNSClient())
	}

	return &DNS{
		tag:           tag,
		hosts:         hosts,
		clients:       clients,
		ctx:           ctx,
		domainMatcher: domainMatcher,
		matcherInfos:  matcherInfos,
	}, nil
}

// Type implements common.HasType.
func (*DNS) Type() interface{} {
	return dns.ClientType()
}

// Start implements common.Runnable.
func (s *DNS) Start() error {
	return nil
}

// Close implements common.Closable.
func (s *DNS) Close() error {
	return nil
}

// IsOwnLink implements proxy.dns.ownLinkVerifier
func (s *DNS) IsOwnLink(ctx context.Context) bool {
	inbound := session.InboundFromContext(ctx)
	return inbound != nil && inbound.Tag == s.tag
}

// LookupIP implements dns.Client.
func (s *DNS) LookupIP(domain string) ([]net.IP, error) {
	return s.lookupIPInternal(domain, IPOption{
		IPv4Enable: true,
		IPv6Enable: true,
	})
}

// LookupIPv4 implements dns.IPv4Lookup.
func (s *DNS) LookupIPv4(domain string) ([]net.IP, error) {
	return s.lookupIPInternal(domain, IPOption{
		IPv4Enable: true,
		IPv6Enable: false,
	})
}

// LookupIPv6 implements dns.IPv6Lookup.
func (s *DNS) LookupIPv6(domain string) ([]net.IP, error) {
	return s.lookupIPInternal(domain, IPOption{
		IPv4Enable: false,
		IPv6Enable: true,
	})
}

func (s *DNS) lookupIPInternal(domain string, option IPOption) ([]net.IP, error) {
	if domain == "" {
		return nil, newError("empty domain name")
	}

	// Normalize the FQDN form query
	if domain[len(domain)-1] == '.' {
		domain = domain[:len(domain)-1]
	}

	// Static host lookup
	switch addrs := s.hosts.Lookup(domain, option); {
	case addrs == nil: // Domain not recorded in static host
		break
	case len(addrs) == 0: // Domain recorded, but no valid IP returned (e.g. IPv4 address with only IPv6 enabled)
		return nil, dns.ErrEmptyResponse
	case len(addrs) == 1 && addrs[0].Family().IsDomain(): // Domain replacement
		newError("domain replaced: ", domain, " -> ", addrs[0].Domain()).WriteToLog()
		domain = addrs[0].Domain()
	default: // Successfully found ip records in static host
		newError("returning ", len(addrs), " IPs for domain ", domain).WriteToLog()
		return toNetIP(addrs)
	}

	// Name servers lookup
	errs := []error{}
	ctx := session.ContextWithInbound(s.ctx, &session.Inbound{Tag: s.tag})
	for _, client := range s.sortClients(domain) {
		ips, err := client.QueryIP(ctx, domain, option)
		if len(ips) > 0 {
			return ips, nil
		}
		if err != nil {
			newError("failed to lookup ip for domain ", domain, " at server ", client.Name()).Base(err).WriteToLog()
			errs = append(errs, err)
		}
		if err != context.Canceled && err != context.DeadlineExceeded && err != errExpectedIPNonMatch {
			return nil, err
		}
	}

	return nil, newError("returning nil for domain ", domain).Base(errors.Combine(errs...))
}

func (s *DNS) sortClients(domain string) []*Client {
	clients := make([]*Client, 0, len(s.clients))
	clientUsed := make([]bool, len(s.clients))
	clientNames := make([]string, 0, len(s.clients))
	domainRules := []string{}

	// Priority domain matching
	for _, match := range s.domainMatcher.Match(domain) {
		info := s.matcherInfos[match]
		client := s.clients[info.clientIdx]
		domainRule := client.domains[info.domainRuleIdx]
		domainRules = append(domainRules, fmt.Sprintf("%s(DNS idx:%d)", domainRule, info.clientIdx))
		if clientUsed[info.clientIdx] {
			continue
		}
		clientUsed[info.clientIdx] = true
		clients = append(clients, client)
		clientNames = append(clientNames, client.Name())
	}

	// Default round-robin query
	for idx, client := range s.clients {
		if clientUsed[idx] {
			continue
		}
		clientUsed[idx] = true
		clients = append(clients, client)
		clientNames = append(clientNames, client.Name())
	}

	if len(domainRules) > 0 {
		newError("domain ", domain, " matches following rules: ", domainRules).AtDebug().WriteToLog()
	}
	if len(clientNames) > 0 {
		newError("domain ", domain, " will use DNS in order: ", clientNames).AtDebug().WriteToLog()
	}
	return clients
}

func init() {
	common.Must(common.RegisterConfig((*Config)(nil), func(ctx context.Context, config interface{}) (interface{}, error) {
		return New(ctx, config.(*Config))
	}))
}
