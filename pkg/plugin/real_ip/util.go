package real_ip

import (
	"net"
	"strings"
)

// copy and modify from https://github.com/gin-gonic/gin/blob/master/gin.go
// under the MIT license
//

// parseIP parse a string representation of an IP and returns a net.IP with the
// minimum byte representation or nil if input is invalid.
func parseIP(ip string) net.IP {
	parsedIP := net.ParseIP(ip)

	if ipv4 := parsedIP.To4(); ipv4 != nil {
		// return ip in a 4-byte representation
		return ipv4
	}

	// return ip in a 16-byte representation or nil
	return parsedIP
}

func prepareTrustedCIDRs(trustedProxies []string) ([]*net.IPNet, error) {
	cidr := make([]*net.IPNet, 0, len(trustedProxies))
	for _, trustedProxy := range trustedProxies {
		if !strings.Contains(trustedProxy, "/") {
			ip := parseIP(trustedProxy)
			if ip == nil {
				return cidr, &net.ParseError{Type: "IP address", Text: trustedProxy}
			}

			switch len(ip) {
			case net.IPv4len:
				trustedProxy += "/32"
			case net.IPv6len:
				trustedProxy += "/128"
			}
		}
		_, cidrNet, err := net.ParseCIDR(trustedProxy)
		if err != nil {
			return cidr, err
		}
		cidr = append(cidr, cidrNet)
	}
	return cidr, nil
}

// isTrustedProxy will check whether the IP address is included in the trusted list according to Engine.trustedCIDRs
func (p *Plugin) isTrustedProxy(ip net.IP) bool {
	if p.trustedCIDRs == nil {
		return false
	}
	for _, cidr := range p.trustedCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// validateHeader will parse X-Forwarded-For header and return the trusted client IP address
func (p *Plugin) validateHeader(header string) (clientIP string, valid bool) {
	if header == "" {
		return "", false
	}
	items := strings.Split(header, ",")
	for i := len(items) - 1; i >= 0; i-- {
		ipStr := strings.TrimSpace(items[i])
		ip := net.ParseIP(ipStr)
		if ip == nil {
			break
		}

		// X-Forwarded-For is appended by proxy
		// Check IPs in reverse order and stop when find untrusted proxy
		if (i == 0) || (!p.isTrustedProxy(ip)) {
			return ipStr, true
		}
	}
	return "", false
}
