package api

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/grandcat/zeroconf"
)

// mDNS advertisement of the companion identity (Phase 6).
//
// The control plane announces _sidey._tcp with TXT id/name so companions
// on the same LAN can discover it with zero configuration. The tailnet
// has no mDNS, so tailnet companions verify candidates against
// /api/v1/server-identity instead (see discovery.go).
//
// Advertisement is best-effort and flag-gated (SIDEY_MDNS_ENABLE=0
// disables it): interfaces are restricted to up, multicast-capable,
// non-loopback NICs, and failures only log.

// discoveryServiceType is the Bonjour service type companions browse.
const discoveryServiceType = "_sidey._tcp"

// discoveryTXT builds the TXT record for an identity. Pure (unit-tested);
// the wire encoding is handled by the zeroconf package.
func discoveryTXT(id, name string) []string {
	return []string{"id=" + id, "v=1", "name=" + name}
}

// discoveryInterfaces selects announcement interfaces: up, multicast
// capable, not loopback. Pure over the input list (unit-tested); callers
// pass net.Interfaces().
func discoveryInterfaces(all []net.Interface) []net.Interface {
	var out []net.Interface
	for _, iface := range all {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if iface.Flags&net.FlagMulticast == 0 {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		out = append(out, iface)
	}
	return out
}

// RunDiscovery announces this control plane until ctx is cancelled. It
// returns immediately after starting the announcement in the background;
// every failure path logs and stops, never crashing the process.
func (s *Server) RunDiscovery(ctx context.Context, port int, enable bool) {
	if !enable {
		s.logger.Info("mdns advertisement disabled")
		return
	}
	if port <= 0 || port > 65535 {
		s.logger.Warn("mdns advertisement skipped: invalid port", "port", port)
		return
	}
	id, name, err := s.serverIdentityRow(ctx)
	if err != nil {
		s.logger.Warn("mdns advertisement skipped: identity unavailable", "error", err)
		return
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		s.logger.Warn("mdns advertisement skipped: interface listing failed", "error", err)
		return
	}
	ifaces = discoveryInterfaces(ifaces)
	if len(ifaces) == 0 {
		s.logger.Warn("mdns advertisement skipped: no multicast interfaces")
		return
	}
	instance := fmt.Sprintf("%s-%s", name, id.String()[:8])
	server, err := zeroconf.Register(
		instance, discoveryServiceType, "local.", port,
		discoveryTXT(id.String(), name), ifaces,
	)
	if err != nil {
		s.logger.Warn("mdns advertisement failed", "error", err)
		return
	}
	s.logger.Info("mdns advertising", "instance", instance,
		"service", discoveryServiceType, "port", port, "interfaces", len(ifaces))
	go func() {
		<-ctx.Done()
		// Goodbye packets flush the record so companions drop us promptly.
		done := make(chan struct{})
		go func() {
			server.Shutdown()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}()
}
