package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
)

const dnsLookupTimeout = 5 * time.Second

var (
	dnsClient    = &dns.Client{Timeout: dnsLookupTimeout}
	dnsTCPClient = &dns.Client{Net: "tcp", Timeout: dnsLookupTimeout}
)

func dnssecRequired() bool {
	return os.Getenv("FMSG_REQUIRE_DNSSEC") == "true"
}

type dnssecQueryResult struct {
	ips []net.IP
	ad  bool
	err error
}

func queryDNSSEC(cfg *dns.ClientConfig, name string, qtype uint16) dnssecQueryResult {
	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(name), qtype)
	msg.SetEdns0(4096, true)

	var lastErr error
	for _, server := range cfg.Servers {
		addr := net.JoinHostPort(server, cfg.Port)
		resp, _, err := dnsClient.Exchange(msg, addr)
		if err == nil && resp != nil && resp.Truncated {
			resp, _, err = dnsTCPClient.Exchange(msg, addr)
		}
		if err != nil {
			lastErr = err
			continue
		}
		if resp == nil {
			lastErr = fmt.Errorf("nil DNS response from %s", addr)
			continue
		}
		if resp.Rcode != dns.RcodeSuccess && resp.Rcode != dns.RcodeNameError {
			lastErr = fmt.Errorf("dns rcode %d from %s", resp.Rcode, addr)
			continue
		}
		var ips []net.IP
		for _, rr := range resp.Answer {
			switch r := rr.(type) {
			case *dns.A:
				if qtype == dns.TypeA && r.A != nil {
					ips = append(ips, r.A)
				}
			case *dns.AAAA:
				if qtype == dns.TypeAAAA && r.AAAA != nil {
					ips = append(ips, r.AAAA)
				}
			}
		}
		return dnssecQueryResult{ips: ips, ad: resp.AuthenticatedData}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no DNS resolvers configured")
	}
	return dnssecQueryResult{err: lastErr}
}

func validateDNSSECResults(fmsgDomain string, resA, resAAAA dnssecQueryResult) ([]net.IP, error) {
	if resA.err != nil && resAAAA.err != nil {
		return nil, fmt.Errorf("dnssec validation failed for %s: A=%v AAAA=%v", fmsgDomain, resA.err, resAAAA.err)
	}
	if resA.err == nil && len(resA.ips) > 0 && !resA.ad {
		return nil, fmt.Errorf("dnssec validation failed for %s: A response missing AD bit", fmsgDomain)
	}
	if resAAAA.err == nil && len(resAAAA.ips) > 0 && !resAAAA.ad {
		return nil, fmt.Errorf("dnssec validation failed for %s: AAAA response missing AD bit", fmsgDomain)
	}

	var verified []net.IP
	if resA.err == nil && resA.ad {
		verified = append(verified, resA.ips...)
	}
	if resAAAA.err == nil && resAAAA.ad {
		verified = append(verified, resAAAA.ips...)
	}
	if len(verified) == 0 {
		if resA.err != nil {
			return nil, fmt.Errorf("dnssec validation failed for %s: A=%v", fmsgDomain, resA.err)
		}
		if resAAAA.err != nil {
			return nil, fmt.Errorf("dnssec validation failed for %s: AAAA=%v", fmsgDomain, resAAAA.err)
		}
		return nil, fmt.Errorf("no DNSSEC-verified A/AAAA records found for %s", fmsgDomain)
	}
	return verified, nil
}

// lookupAuthorisedIPs resolves fmsg.<domain> for A and AAAA records
func lookupAuthorisedIPs(domain string) ([]net.IP, error) {
	fmsgDomain := "fmsg." + domain
	if dnssecRequired() {
		cfg, err := dns.ClientConfigFromFile("/etc/resolv.conf")
		if err != nil {
			return nil, fmt.Errorf("dnssec validation failed for %s: %w", fmsgDomain, err)
		}
		resA := queryDNSSEC(cfg, fmsgDomain, dns.TypeA)
		resAAAA := queryDNSSEC(cfg, fmsgDomain, dns.TypeAAAA)
		return validateDNSSECResults(fmsgDomain, resA, resAAAA)
	}

	ctx, cancel := context.WithTimeout(context.Background(), dnsLookupTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", fmsgDomain)
	if err != nil {
		return nil, fmt.Errorf("DNS lookup for %s failed: %w", fmsgDomain, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no A/AAAA records found for %s", fmsgDomain)
	}

	return ips, nil
}

// getExternalIP discovers this host's external IP address
func getExternalIP() (net.IP, error) {
	services := []string{
		"https://api.ipify.org",
		"https://checkip.amazonaws.com",
		"https://icanhazip.com",
	}
	client := &http.Client{Timeout: 10 * time.Second}
	var lastErr error
	for _, svc := range services {
		resp, err := client.Get(svc)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", svc, err)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("%s: failed to read response: %w", svc, err)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s: unexpected status %d", svc, resp.StatusCode)
			continue
		}
		ip := net.ParseIP(strings.TrimSpace(string(body)))
		if ip == nil {
			lastErr = fmt.Errorf("%s: failed to parse IP from response: %s", svc, string(body))
			continue
		}
		return ip, nil
	}
	return nil, fmt.Errorf("all external IP services failed, last error: %w", lastErr)
}

// verifyDomainIP checks that this host's external IP is present in the
// fmsg.<domain> authorised IP set. Panics if not found.
func verifyDomainIP(domain string) {
	externalIP, err := getExternalIP()
	if err != nil {
		log.Panicf("ERROR: failed to get external IP: %s", err)
	}
	log.Printf("INFO: external IP: %s", externalIP)

	authorisedIPs, err := lookupAuthorisedIPs(domain)
	if err != nil {
		log.Panicf("ERROR: failed to lookup fmsg.%s: %s", domain, err)
	}

	if slices.ContainsFunc(authorisedIPs, externalIP.Equal) {
		log.Printf("INFO: external IP %s found in fmsg.%s authorised IPs", externalIP, domain)
		return
	}

	log.Panicf("ERROR: external IP %s not found in fmsg.%s authorised IPs %v", externalIP, domain, authorisedIPs)
}

// checkDomainIP verifies the external IP is authorised unless
// FMSG_SKIP_DOMAIN_IP_CHECK is set to "true".
func checkDomainIP(domain string) {
	if os.Getenv("FMSG_SKIP_DOMAIN_IP_CHECK") == "true" {
		log.Println("INFO: skipping domain IP verification (FMSG_SKIP_DOMAIN_IP_CHECK=true)")
		return
	}
	verifyDomainIP(domain)
}
