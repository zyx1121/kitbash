package daemon

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/zyx1121/kitbash/internal/manifest"
	"github.com/zyx1121/kitbash/internal/problem"
	"github.com/zyx1121/kitbash/internal/store"
)

// The one name a unit may declare for itself, and what kitbashd asks of it
// before it serves it, see PLAN.md section 2.3.
//
// Under the host's own domain there are derived names and nothing else:
// <name>.<member>.<domain> is the whole namespace, so a declared name there
// would let one member take the operator's apex or another member's address.
// Outside it a name is the member's to declare, and what makes it theirs is
// that it points at this host: the record is the proof, checked at
// registration and again at every start, because a name that stopped pointing
// here is a certificate this host has no business asking for and a member who
// would have to be told twice.

// HostnameResolveTimeout bounds the whole resolution of one declared name. The
// answer is never cached across starts: what a name points at is a fact about
// the world today, and a daemon that remembered yesterday's would serve a name
// its owner had already pointed somewhere else.
const HostnameResolveTimeout = 5 * time.Second

// resolver is the part of net.Resolver kitbashd uses. It is an interface so a
// test answers for a zone rather than the internet; the daemon's own is the
// system resolver, which is what the host's /etc/resolv.conf names.
type resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
}

// underDomain reports whether a name is the host's domain or sits under it.
func underDomain(name, domain string) bool {
	if domain == "" {
		return false
	}
	return name == domain || strings.HasSuffix(name, "."+domain)
}

// isAddressLiteral reports whether a name is an address written out. Four
// numeric labels make a valid DNS name by shape, so this is the rule that
// refuses one rather than the label pattern.
func isAddressLiteral(name string) bool {
	return net.ParseIP(name) != nil
}

// declaredHostname checks the name a unit declared against the two rules that
// do not need the network: it may not be under this host's domain, and it may
// not be an address. Both are invalid-manifest, because neither is a thing a
// permission would make right: the manifest has to change.
//
// It runs at registration alone. What needs the network is proveHostname,
// which runs at registration and at every start.
func (s *Server) declaredHostname(instance string, p store.Process) *problem.Problem {
	if p.Hostname == "" {
		return nil
	}
	if isAddressLiteral(p.Hostname) {
		return problem.InvalidManifestFix(instance,
			fmt.Sprintf("%s is an address and not a host name, so nothing resolves it to this host", p.Hostname),
			"Declare deploy.units[0].hostname as a DNS name whose record points at this host, or remove it and use the name kitbash derives.")
	}
	if underDomain(p.Hostname, s.proxy.domain) {
		return problem.InvalidManifestFix(instance,
			fmt.Sprintf("%s is under %s, which is this host's own domain", p.Hostname, s.proxy.domain),
			fmt.Sprintf("Under %s a Process is served at <name>.<member>.%s and nothing else. Declare a hostname in a domain of your own, or remove it and use the derived name.",
				s.proxy.domain, s.proxy.domain))
	}
	return nil
}

// proveHostname checks that a declared name points at this host. Nothing else
// makes a name the member's: kitbashd serves it and, in acme mode, obtains a
// certificate for it, so a name that resolves somewhere else is a name this
// host answers for nobody.
//
// Two answers are proof. An address of this host among the name's A or AAAA
// records, and a CNAME to the name kitbash derived for this Process, which is
// the member pointing their own name at the address they were given. A Process
// that declares no name proves nothing, because the derived name is this
// host's own by construction.
func (s *Server) proveHostname(ctx context.Context, instance string, p store.Process) *problem.Problem {
	if p.Hostname == "" || !s.proxy.enabled() {
		return nil
	}
	addresses := s.proxy.addresses()
	if len(addresses) == 0 {
		return problem.NotPermitted(instance,
			fmt.Sprintf("kitbashd cannot check that %s points at this host, because this host has no address to check it against", p.Hostname),
			fmt.Sprintf("Set %s in /etc/conf.d/kitbashd to the address the proxy is reached on, then restart kitbashd. In %s mode the host's own addresses are behind the gateway, so the operator names the public one.",
				PublicAddressEnv, TLSGateway))
	}
	lookup, cancel := context.WithTimeout(ctx, HostnameResolveTimeout)
	defer cancel()

	// The derived name first: a member who pointed their own name at the one
	// kitbash gave them has proved it without this host having to know its own
	// public address at all.
	if derived := defaultHost(p.Name, p.Owner, s.proxy.domain); derived != "" {
		if canonical, err := s.resolver.LookupCNAME(lookup, p.Hostname); err == nil &&
			sameName(canonical, derived) {
			return nil
		}
	}

	found, err := s.resolver.LookupIPAddr(lookup, p.Hostname)
	if err != nil {
		return problem.NotPermitted(instance,
			fmt.Sprintf("%s does not resolve, so kitbashd cannot tell that it points at this host", p.Hostname),
			hostnameFix(p, s.proxy.domain, addresses))
	}
	for _, answer := range found {
		got, ok := netip.AddrFromSlice(answer.IP)
		if !ok {
			continue
		}
		for _, mine := range addresses {
			if got.Unmap() == mine {
				return nil
			}
		}
	}
	return problem.NotPermitted(instance,
		fmt.Sprintf("%s resolves to %s, and this host answers on %s",
			p.Hostname, addressList(found), addressText(addresses)),
		hostnameFix(p, s.proxy.domain, addresses))
}

// hostnameFix is what the member does about a name that does not point here.
func hostnameFix(p store.Process, domain string, addresses []netip.Addr) string {
	derived := defaultHost(p.Name, p.Owner, domain)
	if derived == "" {
		return fmt.Sprintf("Point %s at %s, then run the Package again.", p.Hostname, addressText(addresses))
	}
	return fmt.Sprintf("Point %s at %s, or make it a CNAME to %s, then run the Package again.",
		p.Hostname, addressText(addresses), derived)
}

// sameName compares two DNS names, which are equal whatever their case and
// whether or not they carry the root's trailing dot.
func sameName(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}

// addressList is what a name resolved to, for the problem a member reads.
func addressList(found []net.IPAddr) string {
	if len(found) == 0 {
		return "no address"
	}
	out := make([]string, 0, len(found))
	for _, answer := range found {
		out = append(out, answer.IP.String())
	}
	return strings.Join(out, ", ")
}

// addressText is what this host answers on, for the same problem.
func addressText(addresses []netip.Addr) string {
	out := make([]string, 0, len(addresses))
	for _, addr := range addresses {
		out = append(out, addr.String())
	}
	return strings.Join(out, ", ")
}

// addresses are the addresses a name has to resolve to for this host to serve
// it. In acme mode they are this host's own non loopback addresses, because
// that is where the internet reaches the listener, plus the public address if
// the operator named one, which is the host behind a static NAT. In gateway
// mode they are the public address alone: the host's own addresses are behind
// the gateway and a member's record names the gateway, so a host that was not
// told its public address can prove nothing.
func (p *proxy) addresses() []netip.Addr {
	var out []netip.Addr
	if p.public.IsValid() {
		out = append(out, p.public)
	}
	if p.mode == TLSGateway {
		return out
	}
	for _, addr := range p.interfaces() {
		if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
			continue
		}
		if !contains(out, addr) {
			out = append(out, addr)
		}
	}
	return out
}

// contains reports whether an address is already in the list.
func contains(list []netip.Addr, addr netip.Addr) bool {
	for _, held := range list {
		if held == addr {
			return true
		}
	}
	return false
}

// hostInterfaceAddrs reads this host's own addresses. It is read at every
// check rather than at start: an address a host gains is an address a name may
// point at, and nothing here is worth a cache.
func hostInterfaceAddrs() []netip.Addr {
	found, err := net.InterfaceAddrs()
	if err != nil {
		logger.Printf("proxy: could not read this host's own addresses: %v", err)
		return nil
	}
	var out []netip.Addr
	for _, entry := range found {
		var ip net.IP
		switch value := entry.(type) {
		case *net.IPNet:
			ip = value.IP
		case *net.IPAddr:
			ip = value.IP
		}
		if addr, ok := netip.AddrFromSlice(ip); ok {
			out = append(out, addr.Unmap())
		}
	}
	return out
}

// hostnameProblem is what the owner reads through proc_list about a Process
// whose declared name stopped pointing here. It is the shape a secret that
// cannot be resolved has, because it is the same kind of fact: something
// outside the registration changed and the Process did not come back.
func hostnameProblem(prob *problem.Problem) restoreProblem {
	fix := prob.Fix
	if fix == "" {
		fix = "Point the hostname the unit declares at this host, then run the Package again."
	}
	return restoreProblem{
		Detail: "this Process declares a hostname kitbashd could not prove points at this host, so it did not start it: " + prob.Detail,
		Fix:    fix,
	}
}

// ValidDeclaredHostname is the shape half of the rule, which is
// manifest.ValidHostname plus the refusal of an address literal. It is spelled
// here because the manifest package holds the shape and this package holds the
// policy.
func ValidDeclaredHostname(name string) bool {
	return manifest.ValidHostname(name) && !isAddressLiteral(name)
}
