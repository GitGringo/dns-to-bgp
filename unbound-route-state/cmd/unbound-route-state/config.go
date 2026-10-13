// config.go: the daemon's configuration. It defines the Config struct, its
// defaults, and a small hand-written parser for the ini-style state.conf
// file.

package main

import (
	"bufio"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// Config holds the daemon settings read from state.conf. Field comments note
// units and special values; the section markers show which [section] of the
// file each group of fields comes from.
type Config struct {
	// [general]
	ListenSocket               string // UNIX socket path, transport to the Python module
	SocketMode                 int    // e.g. 0660
	MaxInboundConns            int
	ASN                        uint32
	RouterID                   string
	LocalAddress               string // source address for the BGP session
	NextHop                    string // default NEXT_HOP for IPv4 routes; empty = local_address
	NextHopV6                  string // default NEXT_HOP for IPv6 routes; required for any IPv6 route
	Neighbor                   string
	NeighborASN                uint32
	Passive                    bool // wait for the peer to connect instead of dialling out; see internal/bgp.Config.Passive
	RemotePort                 int  // only used when Passive is false
	HoldTimeSeconds            int
	ConnectRetrySeconds        int // active session: re-dial interval while the peer is down
	IdleHoldSeconds            int // pause before reconnecting after a session reset
	GracefulRestart            bool
	GracefulRestartTimeSeconds int
	LLGREnabled                bool
	LLGRStaleSeconds           int
	LogRateLimitPerMin         int    // max messages per minute per warning kind; 0 = unlimited
	ControlSocket              string // UNIX socket for `ctl` (stats/routes); "" = disabled
	ControlSocketMode          int    // e.g. 0660
	TrackFQDN                  bool   // keep fqdn(s) per live IP so `ctl routes` can show them

	// [communities] domains_dir: where "name.list" tokens in communities.conf block
	// headers are looked up (normally the same directory the announcer reads).
	DomainsDir string

	// [keepalive] - keep routes while flows to their IPs are alive (keepalive.go)
	KeepaliveEnabled        bool
	ConntrackCmd            string
	ConntrackArgs           string // extra leading args for conntrack, whitespace separated
	KeepalivePollSeconds    int
	KeepaliveLeaseSeconds   int
	KeepaliveMaxHoldSeconds int // 0 = unlimited
	TCPIdleLimitSeconds     int // 0 = rely on the kernel's own timeout
	TCPEstTimeoutSeconds    int // 0 = read /proc/sys/net/netfilter/nf_conntrack_tcp_timeout_established

	// [unbound] - flush watched names from Unbound's cache at startup (flush.go)
	FlushOnStart       bool
	UnboundControlCmd  string
	UnboundControlArgs string // extra leading args for unbound-control, whitespace separated
	FlushDomainsDir    string // announcer domains.d; "" = only communities.conf patterns
	FlushMaxCommands   int
	FlushPauseMS       int

	// [limits]
	MaxIPEntries      int
	EventQueueSize    int // bounded channel behind state.Store.Submit; overflow is counted and logged by the Store
	MaxDomainPatterns int

	// [ttl]
	GranularitySeconds int
	MinRouteTTLSeconds int

	// [communities]
	RulesFile          string
	RulesReloadSeconds int // poll interval for rules_file hot-reload; 0 = disabled
}

// defaultConfig returns the settings used for any key absent from the file.
func defaultConfig() Config {
	return Config{
		SocketMode:                 0660,
		MaxInboundConns:            2,
		HoldTimeSeconds:            240,
		ConnectRetrySeconds:        10,
		IdleHoldSeconds:            5,
		GracefulRestart:            true,
		GracefulRestartTimeSeconds: 300, // same as a typical BIRD "graceful restart time 300;"
		LLGREnabled:                true,
		LLGRStaleSeconds:           600,
		LogRateLimitPerMin:         60,
		ControlSocket:              "/run/dns-to-bgp/control.sock",
		ControlSocketMode:          0660,
		TrackFQDN:                  true,
		DomainsDir:                 "/opt/dns-to-bgp/domains.d",
		FlushOnStart:               false,
		ConntrackCmd:               "/usr/sbin/conntrack",
		KeepalivePollSeconds:       15,
		KeepaliveLeaseSeconds:      600,
		KeepaliveMaxHoldSeconds:    86400,
		UnboundControlCmd:          "/usr/sbin/unbound-control",
		FlushDomainsDir:            "/opt/dns-to-bgp/domains.d",
		FlushMaxCommands:           500,
		FlushPauseMS:               20,
		RulesReloadSeconds:         5,
		MaxIPEntries:               50000,
		EventQueueSize:             4096, // sized to absorb a burst of several thousand events within tens of milliseconds
		MaxDomainPatterns:          5000,
		GranularitySeconds:         1,
	}
}

// loadConfig reads state.conf from path, starting from defaultConfig. It is a
// minimal parser for the ini subset the file uses: "[section]" headers,
// "key = value" pairs, "#" or ";" comments (full-line or inline), and blank
// lines. It avoids an external ini dependency because the format is so small.
func loadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	defer f.Close()

	cfg := defaultConfig()
	section := ""

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := stripComment(sc.Text())
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if err := cfg.set(section, key, val); err != nil {
			return Config{}, fmt.Errorf("config: [%s] %s: %w", section, key, err)
		}
	}
	if err := sc.Err(); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

// stripComment removes everything from the first "#" or ";" onwards.
func stripComment(line string) string {
	for _, marker := range []string{"#", ";"} {
		if i := strings.Index(line, marker); i >= 0 {
			line = line[:i]
		}
	}
	return line
}

// set applies one key=value pair from the given section. Unknown sections and
// keys are ignored; a value that fails to parse or validate returns an error.
func (c *Config) set(section, key, val string) error {
	switch section {
	case "general":
		switch key {
		case "listen_socket":
			c.ListenSocket = val
		case "socket_mode":
			n, err := strconv.ParseInt(val, 8, 32)
			if err != nil {
				return err
			}
			c.SocketMode = int(n)
		case "control_socket":
			c.ControlSocket = val
		case "control_socket_mode":
			n, err := strconv.ParseInt(val, 8, 32)
			if err != nil {
				return err
			}
			c.ControlSocketMode = int(n)
		case "track_fqdn":
			return setBool(&c.TrackFQDN, val)
		case "max_inbound_conns":
			return setInt(&c.MaxInboundConns, val)
		case "asn":
			return setUint32(&c.ASN, val)
		case "router_id":
			c.RouterID = val
		case "local_address":
			c.LocalAddress = val
		case "next_hop":
			if val == "" { // an empty value means "unset" (the example file ships empty keys)
				c.NextHop = ""
				return nil
			}
			a, err := netip.ParseAddr(val)
			if err != nil || !a.Is4() {
				return fmt.Errorf("next_hop %q must be an IPv4 address (use next_hop_v6 for IPv6)", val)
			}
			c.NextHop = val
		case "next_hop_v6":
			if val == "" {
				c.NextHopV6 = ""
				return nil
			}
			a, err := netip.ParseAddr(val)
			if err != nil || !a.Is6() {
				return fmt.Errorf("next_hop_v6 %q must be an IPv6 address", val)
			}
			c.NextHopV6 = val
		case "neighbor":
			c.Neighbor = val
		case "neighbor_asn":
			return setUint32(&c.NeighborASN, val)
		case "passive":
			return setBool(&c.Passive, val)
		case "remote_port":
			return setInt(&c.RemotePort, val)
		case "connect_retry_seconds":
			if err := setInt(&c.ConnectRetrySeconds, val); err != nil {
				return err
			}
			if c.ConnectRetrySeconds < 2 {
				return fmt.Errorf("connect_retry_seconds must be >= 2 (GoBGP's floor)")
			}
		case "idle_hold_seconds":
			if err := setInt(&c.IdleHoldSeconds, val); err != nil {
				return err
			}
			if c.IdleHoldSeconds < 1 {
				return fmt.Errorf("idle_hold_seconds must be >= 1 (0 would make GoBGP apply its own 30s)")
			}
		case "hold_time_seconds":
			return setInt(&c.HoldTimeSeconds, val)
		case "graceful_restart":
			return setBool(&c.GracefulRestart, val)
		case "graceful_restart_time_seconds":
			return setInt(&c.GracefulRestartTimeSeconds, val)
		case "llgr_enabled":
			return setBool(&c.LLGREnabled, val)
		case "llgr_stale_time":
			return setInt(&c.LLGRStaleSeconds, val)
		case "log_rate_limit_per_min":
			return setInt(&c.LogRateLimitPerMin, val)
		}
	case "limits":
		switch key {
		case "max_ip_entries":
			return setInt(&c.MaxIPEntries, val)
		case "event_queue_size":
			return setInt(&c.EventQueueSize, val)
		case "max_domain_patterns":
			return setInt(&c.MaxDomainPatterns, val)
		}
	case "ttl":
		switch key {
		case "granularity_seconds":
			return setInt(&c.GranularitySeconds, val)
		case "min_route_ttl_seconds":
			return setInt(&c.MinRouteTTLSeconds, val)
		}
	case "keepalive":
		switch key {
		case "enabled":
			return setBool(&c.KeepaliveEnabled, val)
		case "conntrack_command":
			if val == "" {
				return fmt.Errorf("conntrack_command must not be empty")
			}
			c.ConntrackCmd = val
		case "conntrack_args":
			c.ConntrackArgs = val
		case "poll_interval_seconds":
			if err := setInt(&c.KeepalivePollSeconds, val); err != nil {
				return err
			}
			if c.KeepalivePollSeconds < 2 {
				return fmt.Errorf("poll_interval_seconds must be >= 2")
			}
		case "lease_seconds":
			if err := setInt(&c.KeepaliveLeaseSeconds, val); err != nil {
				return err
			}
			if c.KeepaliveLeaseSeconds < 1 {
				return fmt.Errorf("lease_seconds must be >= 1")
			}
		case "max_hold_seconds":
			if err := setInt(&c.KeepaliveMaxHoldSeconds, val); err != nil {
				return err
			}
			if c.KeepaliveMaxHoldSeconds < 0 {
				return fmt.Errorf("max_hold_seconds must be >= 0 (0 = unlimited)")
			}
		case "tcp_idle_limit_seconds":
			if err := setInt(&c.TCPIdleLimitSeconds, val); err != nil {
				return err
			}
			if c.TCPIdleLimitSeconds < 0 {
				return fmt.Errorf("tcp_idle_limit_seconds must be >= 0 (0 = kernel timeout)")
			}
		case "tcp_established_timeout_seconds":
			if err := setInt(&c.TCPEstTimeoutSeconds, val); err != nil {
				return err
			}
			if c.TCPEstTimeoutSeconds < 0 {
				return fmt.Errorf("tcp_established_timeout_seconds must be >= 0 (0 = read the sysctl)")
			}
		}
	case "unbound":
		switch key {
		case "flush_on_start":
			return setBool(&c.FlushOnStart, val)
		case "control_command":
			if val == "" {
				return fmt.Errorf("control_command must not be empty")
			}
			c.UnboundControlCmd = val
		case "control_args":
			c.UnboundControlArgs = val
		case "flush_domains_dir":
			c.FlushDomainsDir = val
		case "flush_max_commands":
			if err := setInt(&c.FlushMaxCommands, val); err != nil {
				return err
			}
			if c.FlushMaxCommands < 1 {
				return fmt.Errorf("flush_max_commands must be >= 1")
			}
		case "flush_pause_ms":
			if err := setInt(&c.FlushPauseMS, val); err != nil {
				return err
			}
			if c.FlushPauseMS < 0 {
				return fmt.Errorf("flush_pause_ms must be >= 0")
			}
		}
	case "communities":
		switch key {
		case "rules_file":
			c.RulesFile = val
		case "domains_dir":
			c.DomainsDir = val
		case "rules_reload_seconds":
			return setInt(&c.RulesReloadSeconds, val)
		}
	}
	return nil // unknown keys are ignored, not fatal
}

// setInt parses val as a decimal int into dst.
func setInt(dst *int, val string) error {
	n, err := strconv.Atoi(val)
	if err != nil {
		return err
	}
	*dst = n
	return nil
}

// setUint32 parses val as a decimal uint32 into dst.
func setUint32(dst *uint32, val string) error {
	n, err := strconv.ParseUint(val, 10, 32)
	if err != nil {
		return err
	}
	*dst = uint32(n)
	return nil
}

// setBool parses yes/true/1 or no/false/0 (case-insensitive) into dst.
func setBool(dst *bool, val string) error {
	switch strings.ToLower(val) {
	case "yes", "true", "1":
		*dst = true
	case "no", "false", "0":
		*dst = false
	default:
		return fmt.Errorf("invalid boolean %q", val)
	}
	return nil
}
