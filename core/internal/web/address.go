// Package web fetches pages for the agent to read.
//
// Reading the web is the one capability where the input is chosen by an
// attacker as a matter of routine. Everything here is arranged around two
// facts: a page is untrusted text however ordinary the site looks, and a URL
// is a way to make this machine open a connection to somewhere of somebody
// else's choosing.
package web

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrRefused is returned for an address this agent will not open.
type ErrRefused struct {
	Address string
	Reason  string
}

func (e *ErrRefused) Error() string {
	return fmt.Sprintf("web: will not fetch %s: %s", e.Address, e.Reason)
}

// CheckURL decides whether an address may be fetched at all.
//
// The check is on the resolved addresses, not on the text of the hostname. A
// name is somebody else's record and can point wherever they like: the classic
// way to reach a machine's own metadata service is a public domain that
// resolves to 169.254.169.254, and refusing the string "localhost" catches
// none of it.
//
// Resolution happening here and again in the fetcher is a known gap — the name
// can change between the two — which is why the fetcher blocks by address at
// request time as well. This is the cheap check that refuses the obvious cases
// before anything opens a connection.
func CheckURL(raw string, resolve func(string) ([]net.IP, error)) (*url.URL, error) {
	// Every refusal below names a redacted form rather than what was passed
	// in. An error goes into the event log, into the model's context, and
	// sometimes back to a chat channel, and a URL is one of the places people
	// put a password.
	safe := RedactAddress(raw)

	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, &ErrRefused{Address: safe, Reason: "it is not a URL"}
	}

	switch parsed.Scheme {
	case "http", "https":
	default:
		// file:// reads this machine's disk through a door meant for the web,
		// and everything else is a scheme somebody registered on this desktop.
		return nil, &ErrRefused{
			Address: safe,
			Reason:  fmt.Sprintf("%q is not a scheme this fetches; use http or https", parsed.Scheme),
		}
	}

	host := parsed.Hostname()
	if host == "" {
		return nil, &ErrRefused{Address: safe, Reason: "it names no host"}
	}
	if parsed.User != nil {
		// Credentials in a URL are a way to hand a site somebody's password,
		// and a way to make a host look like one thing and be another.
		return nil, &ErrRefused{
			Address: safe,
			Reason:  "it carries credentials, which are not sent on somebody else's behalf",
		}
	}

	if resolve == nil {
		resolve = net.LookupIP
	}

	addresses, err := resolve(host)
	if err != nil {
		return nil, &ErrRefused{Address: safe, Reason: fmt.Sprintf("its host does not resolve: %v", err)}
	}
	if len(addresses) == 0 {
		return nil, &ErrRefused{Address: safe, Reason: "its host resolves to nothing"}
	}

	// Every address, not the first. A name that resolves to one public address
	// and one private one is the whole trick.
	for _, address := range addresses {
		if reason, private := IsPrivate(address); private {
			return nil, &ErrRefused{
				Address: safe,
				Reason:  fmt.Sprintf("%s is %s", address, reason),
			}
		}
	}

	return parsed, nil
}

// IsPrivate reports whether an address belongs to this machine, this network,
// or the places a cloud host keeps its credentials.
func IsPrivate(address net.IP) (reason string, private bool) {
	switch {
	case address.IsLoopback():
		return "this machine", true
	case address.IsPrivate():
		return "a private network", true
	case address.IsLinkLocalUnicast(), address.IsLinkLocalMulticast():
		// 169.254.169.254 lives here, and it hands out cloud credentials to
		// anything that asks.
		return "link-local, where cloud metadata services live", true
	case address.IsUnspecified():
		return "unspecified", true
	case address.IsMulticast():
		return "multicast", true
	case isSharedAddressSpace(address):
		return "carrier-grade NAT space", true
	}

	return "", false
}

// isSharedAddressSpace covers 100.64.0.0/10, which is neither public nor
// covered by IsPrivate, and is where a good deal of internal infrastructure
// now lives.
func isSharedAddressSpace(address net.IP) bool {
	v4 := address.To4()
	return v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
}

// RedactAddress removes any credentials from a URL so it can be logged.
//
// It works on the text rather than on a parsed URL, because the address that
// most needs redacting is the one malformed enough that parsing it failed.
func RedactAddress(raw string) string {
	raw = strings.TrimSpace(raw)

	schemeEnd := strings.Index(raw, "://")
	if schemeEnd < 0 {
		return raw
	}

	authorityStart := schemeEnd + len("://")
	authorityEnd := strings.IndexAny(raw[authorityStart:], "/?#")
	if authorityEnd < 0 {
		authorityEnd = len(raw)
	} else {
		authorityEnd += authorityStart
	}

	authority := raw[authorityStart:authorityEnd]
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return raw
	}

	return raw[:authorityStart] + "[redacted]@" + authority[at+1:] + raw[authorityEnd:]
}
