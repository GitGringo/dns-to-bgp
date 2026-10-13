package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"unbound-route-state/internal/state"
)

// Traffic keepalive.
//
// Problem: a client keeps talking to an IP long after the DNS TTL that put it
// into the cache has lapsed (TCP/QUIC session, NAT'ed UDP). When the route is
// withdrawn, the packets leave via another path mid-session. min_route_ttl_seconds
// is a blunt answer (everything is held that long, active or not).
//
// Mechanism: every poll_interval the daemon lists the IPs in its cache, asks the
// kernel's connection tracking which of them currently have flows (this host
// forwards and NATs the clients' traffic, so the flows are in conntrack), and
// tells the Store "a live flow to these IPs was seen now" (Store.NoteTraffic).
// The Store then keeps each such route for lease past the last observation,
// capped by max_hold per keepalive streak (see state.Config.KeepLease).
//
// Source: `conntrack -L` (conntrack-tools), run as a child process and parsed
// as a stream. It inherits CAP_NET_ADMIN from the daemon (AmbientCapabilities
// in the unit file). Polling every few seconds is a deliberate choice over
// conntrack events: routes are withdrawn on a minutes-scale, so event latency
// buys nothing, while events can be lost under load and still need a full dump.
//
// What counts as a live flow to an IP: an entry whose ORIGINAL destination (or
// reply source) is that IP and which is
//   - TCP in SYN_SENT, SYN_RECV or ESTABLISHED (TIME_WAIT, FIN_WAIT, CLOSE...
//     do not count); optionally only if it carried a packet within
//     tcp_idle_limit - the entry's remaining timeout resets on every packet, so
//     idle = nf_conntrack_tcp_timeout_established - remaining;
//   - any other protocol (UDP, ICMP, ...): the entry exists. Their kernel
//     timeouts are short, so existence already means recent traffic.
// By default no extra idle limit is applied: the kernel's own timeout
// (nf_conntrack_tcp_timeout_established, read from /proc/sys at startup) is the
// idle limit.

const sysctlTCPEstablished = "/proc/sys/net/netfilter/nf_conntrack_tcp_timeout_established"

type keepaliveSettings struct {
	Command       string
	Args          []string // extra args placed before "-L"
	Interval      time.Duration
	TCPIdleLimit  time.Duration // 0 = off: the kernel timeout decides
	TCPEstTimeout time.Duration // 0 = read the sysctl (only needed when TCPIdleLimit > 0)
}

// streamRunner starts a command and hands its stdout to consume. Streaming
// keeps memory flat however large the conntrack table is.
type streamRunner func(ctx context.Context, name string, args []string, consume func(io.Reader) error) error

func execStream(ctx context.Context, name string, args []string, consume func(io.Reader) error) error {
	cmd := exec.CommandContext(ctx, name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, max: 4096}
	if err := cmd.Start(); err != nil {
		return err
	}
	cerr := consume(stdout)
	_, _ = io.Copy(io.Discard, stdout) // let the child finish even if consume stopped early
	werr := cmd.Wait()
	if werr != nil {
		return fmt.Errorf("%w: %s", werr, oneLine(stderr.Bytes()))
	}
	return cerr
}

// limitedWriter keeps at most max bytes (stderr of the child is only for messages).
type limitedWriter struct {
	w   *bytes.Buffer
	max int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if room := l.max - l.w.Len(); room > 0 {
		if len(p) > room {
			l.w.Write(p[:room])
		} else {
			l.w.Write(p)
		}
	}
	return len(p), nil
}

func readKernelTCPEstablished(path string) (time.Duration, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("unexpected content of %s: %q", path, strings.TrimSpace(string(b)))
	}
	return time.Duration(n) * time.Second, nil
}

func addrToIP(a netip.Addr) state.IP {
	a = a.Unmap()
	if a.Is4() {
		b := a.As4()
		return state.IPv4ToIP(b[0], b[1], b[2], b[3])
	}
	return state.IPv6ToIP(a.As16())
}

type ctOpts struct {
	tcpEstTimeout time.Duration // kernel value; used only together with tcpIdleLimit
	tcpIdleLimit  time.Duration // 0 = no extra idle filter
}

type ctLine struct {
	proto     string
	remaining int64  // seconds until the kernel drops the entry
	tcpState  string // "" for protocols without a state column
	origDst   netip.Addr
	replySrc  netip.Addr
}

// parseCTLine parses one line of `conntrack -L`, for example
//
//	tcp 6 10799 ESTABLISHED src=10.0.0.5 dst=93.184.216.34 sport=51234 dport=443 src=93.184.216.34 dst=203.0.113.5 sport=443 dport=51234 [ASSURED] mark=0 use=1
//	udp 17 29 src=10.0.0.5 dst=8.8.8.8 sport=40000 dport=53 src=8.8.8.8 dst=203.0.113.5 sport=53 dport=40000 mark=0 use=1
//
// The first src=/dst= pair is the original direction, the second the reply.
// (The "-o extended" prefix "ipv4 2" is tolerated.) Lines that are not entries
// (e.g. the summary conntrack prints) report ok=false.
func parseCTLine(line string) (ct ctLine, ok bool) {
	f := strings.Fields(line)
	if len(f) >= 2 && (f[0] == "ipv4" || f[0] == "ipv6") {
		f = f[2:]
	}
	if len(f) < 4 {
		return ct, false
	}
	rem, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return ct, false
	}
	ct.proto, ct.remaining = f[0], rem
	i := 3
	if !strings.ContainsAny(f[3], "=[") {
		ct.tcpState, i = f[3], 4
	}
	srcs := 0
	for ; i < len(f); i++ {
		k, v, found := strings.Cut(f[i], "=")
		if !found {
			continue
		}
		switch k {
		case "dst":
			if !ct.origDst.IsValid() {
				ct.origDst, _ = netip.ParseAddr(v)
			}
		case "src":
			if srcs++; srcs == 2 {
				ct.replySrc, _ = netip.ParseAddr(v)
			}
		}
	}
	return ct, ct.origDst.IsValid()
}

// liveFlow decides whether a conntrack entry counts as a live flow.
func (o ctOpts) liveFlow(ct ctLine) bool {
	if ct.proto != "tcp" {
		return true
	}
	switch ct.tcpState {
	case "SYN_SENT", "SYN_RECV":
		return true
	case "ESTABLISHED":
		if o.tcpIdleLimit > 0 && o.tcpEstTimeout > 0 {
			idle := int64(o.tcpEstTimeout/time.Second) - ct.remaining
			return idle <= int64(o.tcpIdleLimit/time.Second)
		}
		return true
	}
	return false
}

// parseConntrack counts live flows per watched IP in the `conntrack -L` text
// read from r. Each entry is counted once per IP.
func parseConntrack(r io.Reader, watch map[state.IP]struct{}, o ctOpts) (map[state.IP]int, error) {
	out := map[state.IP]int{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		ct, ok := parseCTLine(sc.Text())
		if !ok || !o.liveFlow(ct) {
			continue
		}
		var first state.IP
		haveFirst := false
		for _, a := range []netip.Addr{ct.origDst, ct.replySrc} {
			if !a.IsValid() {
				continue
			}
			ip := addrToIP(a)
			if _, w := watch[ip]; !w {
				continue
			}
			if haveFirst && ip == first {
				continue
			}
			out[ip]++
			first, haveFirst = ip, true
		}
	}
	return out, sc.Err()
}

type trafficStore interface {
	ListIPs(ctx context.Context) ([]state.IP, error)
	NoteTraffic(ctx context.Context, items []state.Traffic) (int, error)
}

type pollResult struct {
	Watched, WithTraffic, Extended int
}

// pollTraffic is one keepalive cycle. A conntrack failure in one address
// family does not discard what the other family found: the observations are
// still reported, and the error is returned for logging.
func pollTraffic(ctx context.Context, ks keepaliveSettings, st trafficStore, run streamRunner, now func() time.Time) (pollResult, error) {
	var res pollResult
	ips, err := st.ListIPs(ctx)
	if err != nil {
		return res, fmt.Errorf("list cached IPs: %w", err)
	}
	res.Watched = len(ips)
	if len(ips) == 0 {
		return res, nil
	}
	watch := make(map[state.IP]struct{}, len(ips))
	var hasV4, hasV6 bool
	for _, ip := range ips {
		watch[ip] = struct{}{}
		if ip.IsIPv4() {
			hasV4 = true
		} else {
			hasV6 = true
		}
	}

	opts := ctOpts{tcpEstTimeout: ks.TCPEstTimeout, tcpIdleLimit: ks.TCPIdleLimit}
	counts := map[state.IP]int{}
	var firstErr error
	for _, fam := range []struct {
		name string
		need bool
	}{{"ipv4", hasV4}, {"ipv6", hasV6}} {
		if !fam.need {
			continue
		}
		args := append(append([]string(nil), ks.Args...), "-L", "-f", fam.name)
		err := run(ctx, ks.Command, args, func(r io.Reader) error {
			c, perr := parseConntrack(r, watch, opts)
			for ip, n := range c {
				counts[ip] += n
			}
			return perr
		})
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("conntrack %s: %w", fam.name, err)
		}
	}

	if len(counts) > 0 {
		t := uint32(now().Unix())
		items := make([]state.Traffic, 0, len(counts))
		for ip, n := range counts {
			if n > 0 {
				items = append(items, state.Traffic{IP: ip, LastSeen: t})
			}
		}
		res.WithTraffic = len(items)
		ext, nerr := st.NoteTraffic(ctx, items)
		if nerr != nil && firstErr == nil {
			firstErr = fmt.Errorf("report traffic: %w", nerr)
		}
		res.Extended = ext
	}
	return res, firstErr
}

// runKeepalive polls until ctx is done.
func runKeepalive(ctx context.Context, ks keepaliveSettings, st trafficStore, run streamRunner, log logger) {
	if ks.TCPIdleLimit > 0 && ks.TCPEstTimeout == 0 {
		d, err := readKernelTCPEstablished(sysctlTCPEstablished)
		if err != nil {
			log.Warnf("keepalive: cannot read the kernel's TCP established timeout, tcp_idle_limit is ignored: %v", err)
			ks.TCPIdleLimit = 0
		} else {
			ks.TCPEstTimeout = d
		}
	}
	idle := "kernel timeout"
	if ks.TCPIdleLimit > 0 {
		idle = fmt.Sprintf("%s (kernel timeout %s)", ks.TCPIdleLimit, ks.TCPEstTimeout)
	}
	log.Infof("keepalive: polling conntrack every %s, TCP idle limit: %s; every route lives at least %s so that a poll always sees it", ks.Interval, idle, 2*ks.Interval)

	t := time.NewTicker(ks.Interval)
	defer t.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, flushCmdTimeout)
		res, err := pollTraffic(pctx, ks, st, run, time.Now)
		cancel()

		ctr.kaLastPollAt.Store(time.Now().Unix())
		ctr.kaLastMatched.Store(int64(res.WithTraffic))
		if err != nil {
			ctr.kaPollsFailed.Add(1)
			ctr.kaLastErr.Store(err.Error())
			failing = true
			log.Warnf("keepalive: conntrack poll failed (routes fall back to DNS TTL / min_route_ttl_seconds): %v", err)
			continue
		}
		ctr.kaPollsOK.Add(1)
		if failing {
			failing = false
			ctr.kaLastErr.Store("")
			log.Infof("keepalive: conntrack polling works again")
		}
	}
}
