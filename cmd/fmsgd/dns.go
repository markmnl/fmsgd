package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/miekg/dns"
)

func dnssecRequired() bool {
	return os.Getenv("FMSG_REQUIRE_DNSSEC") == "true"
}

func resolverAuthenticatedData(name string, qtype uint16) (bool, error) {
	cfg, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil {
		return false, err
	}

	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(name), qtype)
	msg.SetEdns0(4096, true)

	client := &dns.Client{Timeout: 5 * time.Second}
	var lastErr error
	for _, server := range cfg.Servers {
		addr := net.JoinHostPort(server, cfg.Port)
		resp, _, err := client.Exchange(msg, addr)
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
		return resp.AuthenticatedData, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no DNS resolvers configured")
	}
	return false, lastErr
}

// lookupAuthorisedIPs resolves fmsg.<domain> for A and AAAA records
func lookupAuthorisedIPs(domain string) ([]net.IP, error) {
	fmsgDomain := "fmsg." + domain
	ips, err := net.LookupIP(fmsgDomain)
	if err != nil {
		return nil, fmt.Errorf("DNS lookup for %s failed: %w", fmsgDomain, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no A/AAAA records found for %s", fmsgDomain)
	}

	if dnssecRequired() {
		adA, errA := resolverAuthenticatedData(fmsgDomain, dns.TypeA)
		adAAAA, errAAAA := resolverAuthenticatedData(fmsgDomain, dns.TypeAAAA)
		if !adA && !adAAAA {
			if errA != nil && errAAAA != nil {
				return nil, fmt.Errorf("dnssec validation failed for %s: A=%v AAAA=%v", fmsgDomain, errA, errAAAA)
			}
			return nil, fmt.Errorf("dnssec validation failed for %s: resolver did not set AD bit", fmsgDomain)
		}
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

	for _, ip := range authorisedIPs {
		if externalIP.Equal(ip) {
			log.Printf("INFO: external IP %s found in fmsg.%s authorised IPs", externalIP, domain)
			return
		}
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
