package conf

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/golang/protobuf/proto"
	"v2ray.com/core/app/router"
	"v2ray.com/core/common/net"
	"v2ray.com/core/common/platform/filesystem"
)

type RouterRulesConfig struct {
	RuleList       []json.RawMessage `json:"rules"`
	DomainStrategy string            `json:"domainStrategy"`
}

type BalancingRule struct {
	Tag       string     `json:"tag"`
	Selectors StringList `json:"selector"`
}

func (r *BalancingRule) Build() (*router.BalancingRule, error) {
	if r.Tag == "" {
		return nil, newError("empty balancer tag")
	}
	if len(r.Selectors) == 0 {
		return nil, newError("empty selector list")
	}

	return &router.BalancingRule{
		Tag:              r.Tag,
		OutboundSelector: []string(r.Selectors),
	}, nil
}

type RouterConfig struct {
	Settings       *RouterRulesConfig `json:"settings"` // Deprecated
	RuleList       []json.RawMessage  `json:"rules"`
	DomainStrategy *string            `json:"domainStrategy"`
	Balancers      []*BalancingRule   `json:"balancers"`
}

func (c *RouterConfig) getDomainStrategy() router.Config_DomainStrategy {
	ds := ""
	if c.DomainStrategy != nil {
		ds = *c.DomainStrategy
	} else if c.Settings != nil {
		ds = c.Settings.DomainStrategy
	}

	switch strings.ToLower(ds) {
	case "alwaysip", "always_ip", "always-ip":
		return router.Config_UseIp
	case "ipifnonmatch", "ip_if_non_match", "ip-if-non-match":
		return router.Config_IpIfNonMatch
	case "ipondemand", "ip_on_demand", "ip-on-demand":
		return router.Config_IpOnDemand
	default:
		return router.Config_AsIs
	}
}

func (c *RouterConfig) Build() (*router.Config, error) {
	config := new(router.Config)
	config.DomainStrategy = c.getDomainStrategy()

	var rawRuleList []json.RawMessage
	if c != nil {
		rawRuleList = c.RuleList
		if c.Settings != nil {
			c.RuleList = append(c.RuleList, c.Settings.RuleList...)
			rawRuleList = c.RuleList
		}
	}

	for _, rawRule := range rawRuleList {
		rule, err := ParseRule(rawRule)
		if err != nil {
			return nil, err
		}
		config.Rule = append(config.Rule, rule)
	}
	for _, rawBalancer := range c.Balancers {
		balancer, err := rawBalancer.Build()
		if err != nil {
			return nil, err
		}
		config.BalancingRule = append(config.BalancingRule, balancer)
	}
	return config, nil
}

type RouterRule struct {
	Type        string `json:"type"`
	OutboundTag string `json:"outboundTag"`
	BalancerTag string `json:"balancerTag"`
}

func ParseIP(s string) (*router.CIDR, error) {
	var addr, mask string
	i := strings.Index(s, "/")
	if i < 0 {
		addr = s
	} else {
		addr = s[:i]
		mask = s[i+1:]
	}
	ip := net.ParseAddress(addr)
	switch ip.Family() {
	case net.AddressFamilyIPv4:
		bits := uint32(32)
		if len(mask) > 0 {
			bits64, err := strconv.ParseUint(mask, 10, 32)
			if err != nil {
				return nil, newError("invalid network mask for router: ", mask).Base(err)
			}
			bits = uint32(bits64)
		}
		if bits > 32 {
			return nil, newError("invalid network mask for router: ", bits)
		}
		return &router.CIDR{
			Ip:     []byte(ip.IP()),
			Prefix: bits,
		}, nil
	case net.AddressFamilyIPv6:
		bits := uint32(128)
		if len(mask) > 0 {
			bits64, err := strconv.ParseUint(mask, 10, 32)
			if err != nil {
				return nil, newError("invalid network mask for router: ", mask).Base(err)
			}
			bits = uint32(bits64)
		}
		if bits > 128 {
			return nil, newError("invalid network mask for router: ", bits)
		}
		return &router.CIDR{
			Ip:     []byte(ip.IP()),
			Prefix: bits,
		}, nil
	default:
		return nil, newError("unsupported address for router: ", s)
	}
}

func loadGeoIP(country string) ([]*router.CIDR, error) {
	return loadIP("geoip.dat", country)
}

func loadIP(filename, country string) ([]*router.CIDR, error) {
	geoipBytes, err := filesystem.ReadAsset(filename)
	if err != nil {
		return nil, newError("failed to open file: ", filename).Base(err)
	}
	var geoipList router.GeoIPList
	if err := proto.Unmarshal(geoipBytes, &geoipList); err != nil {
		return nil, err
	}

	for _, geoip := range geoipList.Entry {
		if strings.EqualFold(geoip.CountryCode, country) {
			return geoip.Cidr, nil
		}
	}

	return nil, newError("country not found in ", filename, ": ", country)
}

func loadSite(filename, list string) ([]*router.Domain, error) {
	geositeBytes, err := filesystem.ReadAsset(filename)
	if err != nil {
		return nil, newError("failed to open file: ", filename).Base(err)
	}
	var geositeList router.GeoSiteList
	if err := proto.Unmarshal(geositeBytes, &geositeList); err != nil {
		return nil, err
	}

	for _, site := range geositeList.Entry {
		if strings.EqualFold(site.CountryCode, list) {
			return site.Domain, nil
		}
	}

	return nil, newError("list not found in ", filename, ": ", list)
}

type AttributeMatcher interface {
	Match(*router.Domain) bool
}

type BooleanMatcher string

func (m BooleanMatcher) Match(domain *router.Domain) bool {
	for _, attr := range domain.Attribute {
		if strings.EqualFold(attr.GetKey(), string(m)) {
			return true
		}
	}
	return false
}

type AttributeList struct {
	matcher []AttributeMatcher
}

func (al *AttributeList) Match(domain *router.Domain) bool {
	for _, matcher := range al.matcher {
		if !matcher.Match(domain) {
			return false
		}
	}
	return true
}

func (al *AttributeList) IsEmpty() bool {
	return len(al.matcher) == 0
}

func parseAttrs(attrs []string) *AttributeList {
	al := new(AttributeList)
	for _, attr := range attrs {
		trimmedAttr := strings.ToLower(strings.TrimSpace(attr))
		if len(trimmedAttr) == 0 {
			continue
		}
		al.matcher = append(al.matcher, BooleanMatcher(trimmedAttr))
	}
	return al
}

func loadGeositeWithAttr(file string, siteWithAttr string) ([]*router.Domain, error) {
	parts := strings.Split(siteWithAttr, "@")
	if len(parts) == 0 {
		return nil, newError("empty rule")
	}
	list := strings.TrimSpace(parts[0])
	attrVal := parts[1:]

	if len(list) == 0 {
		return nil, newError("empty listname in rule: ", siteWithAttr)
	}

	domains, err := loadSite(file, list)
	if err != nil {
		return nil, err
	}

	attrs := parseAttrs(attrVal)
	if attrs.IsEmpty() {
		if strings.Contains(siteWithAttr, "@") {
			newError("empty attribute list: ", siteWithAttr)
		}
		return domains, nil
	}

	filteredDomains := make([]*router.Domain, 0, len(domains))
	hasAttrMatched := false
	for _, domain := range domains {
		if attrs.Match(domain) {
			hasAttrMatched = true
			filteredDomains = append(filteredDomains, domain)
		}
	}
	if !hasAttrMatched {
		newError("attribute match no rule: geosite:", siteWithAttr)
	}

	return filteredDomains, nil
}

func parseDomainRule(domain string) ([]*router.Domain, error) {
	if strings.HasPrefix(domain, "geosite:") {
		list := domain[8:]
		if len(list) == 0 {
			return nil, newError("empty listname in rule: ", domain)
		}
		domains, err := loadGeositeWithAttr("geosite.dat", list)
		if err != nil {
			return nil, newError("failed to load geosite: ", list).Base(err)
		}

		return domains, nil
	}

	var isExtDatFile = 0
	{
		const prefix = "ext:"
		if strings.HasPrefix(domain, prefix) {
			isExtDatFile = len(prefix)
		}
		const prefixQualified = "ext-domain:"
		if strings.HasPrefix(domain, prefixQualified) {
			isExtDatFile = len(prefixQualified)
		}
	}

	if isExtDatFile != 0 {
		kv := strings.Split(domain[isExtDatFile:], ":")
		if len(kv) != 2 {
			return nil, newError("invalid external resource: ", domain)
		}
		filename := kv[0]
		list := kv[1]
		domains, err := loadGeositeWithAttr(filename, list)
		if err != nil {
			return nil, newError("failed to load external geosite: ", list, " from ", filename).Base(err)
		}

		return domains, nil
	}

	domainRule := new(router.Domain)
	switch {
	case strings.HasPrefix(domain, "regexp:"):
		regexpVal := domain[7:]
		if len(regexpVal) == 0 {
			return nil, newError("empty regexp type of rule: ", domain)
		}
		domainRule.Type = router.Domain_Regex
		domainRule.Value = regexpVal

	case strings.HasPrefix(domain, "domain:"):
		domainName := domain[7:]
		if len(domainName) == 0 {
			return nil, newError("empty domain type of rule: ", domain)
		}
		domainRule.Type = router.Domain_Domain
		domainRule.Value = domainName

	case strings.HasPrefix(domain, "full:"):
		fullVal := domain[5:]
		if len(fullVal) == 0 {
			return nil, newError("empty full domain type of rule: ", domain)
		}
		domainRule.Type = router.Domain_Full
		domainRule.Value = fullVal

	case strings.HasPrefix(domain, "keyword:"):
		keywordVal := domain[8:]
		if len(keywordVal) == 0 {
			return nil, newError("empty keyword type of rule: ", domain)
		}
		domainRule.Type = router.Domain_Plain
		domainRule.Value = keywordVal

	case strings.HasPrefix(domain, "dotless:"):
		domainRule.Type = router.Domain_Regex
		switch substr := domain[8:]; {
		case substr == "":
			domainRule.Value = "^[^.]*$"
		case !strings.Contains(substr, "."):
			domainRule.Value = "^[^.]*" + substr + "[^.]*$"
		default:
			return nil, newError("substr in dotless rule should not contain a dot: ", substr)
		}

	default:
		domainRule.Type = router.Domain_Plain
		domainRule.Value = domain
	}
	return []*router.Domain{domainRule}, nil
}

func toCidrList(ips StringList) ([]*router.GeoIP, error) {
	var geoipList []*router.GeoIP
	var customCidrs []*router.CIDR

	for _, ip := range ips {
		if strings.HasPrefix(ip, "geoip:") {
			country := ip[6:]
			if len(country) == 0 {
				return nil, newError("empty country name in rule")
			}
			geoip, err := loadGeoIP(country)
			if err != nil {
				return nil, newError("failed to load geoip: ", country).Base(err)
			}

			geoipList = append(geoipList, &router.GeoIP{
				CountryCode: strings.ToUpper(country),
				Cidr:        geoip,
			})

			continue
		}

		var isExtDatFile = 0
		{
			const prefix = "ext:"
			if strings.HasPrefix(ip, prefix) {
				isExtDatFile = len(prefix)
			}
			const prefixQualified = "ext-ip:"
			if strings.HasPrefix(ip, prefixQualified) {
				isExtDatFile = len(prefixQualified)
			}
		}

		if isExtDatFile != 0 {
			kv := strings.Split(ip[isExtDatFile:], ":")
			if len(kv) != 2 {
				return nil, newError("invalid external resource: ", ip)
			}

			filename := kv[0]
			country := kv[1]
			if len(filename) == 0 || len(country) == 0 {
				return nil, newError("empty filename or empty country in rule")
			}
			geoip, err := loadIP(filename, country)
			if err != nil {
				return nil, newError("failed to load geoip: ", country, " from ", filename).Base(err)
			}

			geoipList = append(geoipList, &router.GeoIP{
				CountryCode: strings.ToUpper(filename + "_" + country),
				Cidr:        geoip,
			})

			continue
		}

		ipRule, err := ParseIP(ip)
		if err != nil {
			return nil, newError("invalid IP: ", ip).Base(err)
		}
		customCidrs = append(customCidrs, ipRule)
	}

	if len(customCidrs) > 0 {
		geoipList = append(geoipList, &router.GeoIP{
			Cidr: customCidrs,
		})
	}

	return geoipList, nil
}

func parseFieldRule(msg json.RawMessage) (*router.RoutingRule, error) {
	type RawFieldRule struct {
		RouterRule
		Domain     *StringList  `json:"domain"`
		Domains    *StringList  `json:"domains"`
		IP         *StringList  `json:"ip"`
		Port       *PortList    `json:"port"`
		Network    *NetworkList `json:"network"`
		SourceIP   *StringList  `json:"source"`
		SourcePort *PortList    `json:"sourcePort"`
		User       *StringList  `json:"user"`
		InboundTag *StringList  `json:"inboundTag"`
		Protocols  *StringList  `json:"protocol"`
		Attributes string       `json:"attrs"`
	}
	rawFieldRule := new(RawFieldRule)
	err := json.Unmarshal(msg, rawFieldRule)
	if err != nil {
		return nil, err
	}

	rule := new(router.RoutingRule)
	switch {
	case len(rawFieldRule.OutboundTag) > 0:
		rule.TargetTag = &router.RoutingRule_Tag{
			Tag: rawFieldRule.OutboundTag,
		}
	case len(rawFieldRule.BalancerTag) > 0:
		rule.TargetTag = &router.RoutingRule_BalancingTag{
			BalancingTag: rawFieldRule.BalancerTag,
		}
	default:
		return nil, newError("neither outboundTag nor balancerTag is specified in routing rule")
	}

	if rawFieldRule.Domain != nil {
		for _, domain := range *rawFieldRule.Domain {
			rules, err := parseDomainRule(domain)
			if err != nil {
				return nil, newError("failed to parse domain rule: ", domain).Base(err)
			}
			rule.Domain = append(rule.Domain, rules...)
		}
	}

	if rawFieldRule.Domains != nil {
		for _, domain := range *rawFieldRule.Domains {
			rules, err := parseDomainRule(domain)
			if err != nil {
				return nil, newError("failed to parse domain rule: ", domain).Base(err)
			}
			rule.Domain = append(rule.Domain, rules...)
		}
	}

	if rawFieldRule.IP != nil {
		geoipList, err := toCidrList(*rawFieldRule.IP)
		if err != nil {
			return nil, err
		}
		rule.Geoip = geoipList
	}

	if rawFieldRule.Port != nil {
		rule.PortList = rawFieldRule.Port.Build()
	}

	if rawFieldRule.Network != nil {
		rule.Networks = rawFieldRule.Network.Build()
	}

	if rawFieldRule.SourceIP != nil {
		geoipList, err := toCidrList(*rawFieldRule.SourceIP)
		if err != nil {
			return nil, err
		}
		rule.SourceGeoip = geoipList
	}

	if rawFieldRule.SourcePort != nil {
		rule.SourcePortList = rawFieldRule.SourcePort.Build()
	}

	if rawFieldRule.User != nil {
		for _, s := range *rawFieldRule.User {
			rule.UserEmail = append(rule.UserEmail, s)
		}
	}

	if rawFieldRule.InboundTag != nil {
		for _, s := range *rawFieldRule.InboundTag {
			rule.InboundTag = append(rule.InboundTag, s)
		}
	}

	if rawFieldRule.Protocols != nil {
		for _, s := range *rawFieldRule.Protocols {
			rule.Protocol = append(rule.Protocol, s)
		}
	}

	if len(rawFieldRule.Attributes) > 0 {
		rule.Attributes = rawFieldRule.Attributes
	}

	return rule, nil
}

func ParseRule(msg json.RawMessage) (*router.RoutingRule, error) {
	rawRule := new(RouterRule)
	err := json.Unmarshal(msg, rawRule)
	if err != nil {
		return nil, newError("invalid router rule").Base(err)
	}
	if strings.EqualFold(rawRule.Type, "field") {
		fieldrule, err := parseFieldRule(msg)
		if err != nil {
			return nil, newError("invalid field rule").Base(err)
		}
		return fieldrule, nil
	}

	return nil, newError("unknown router rule type: ", rawRule.Type)
}
