package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

const ctlUsageHead = `usage: unbound-route-state ctl [options] <command> [command flags]

Queries a running unbound-route-state daemon over its control socket.
Exit status: 0 ok, 1 error reply from the daemon, 2 usage / cannot connect.

options:
`

const defaultConfigPath = "/opt/dns-to-bgp/conf.d/state.conf"

// runCtl is the `unbound-route-state ctl ...` client: it sends one command
// line to the running daemon's control socket and prints the reply
// verbatim. All formatting (text / JSON) happens in the daemon, so this
// stays a thin pipe - `socat - UNIX-CONNECT:<socket>` is equivalent.
//
//	unbound-route-state ctl [-socket PATH] [-config FILE] <command> [flags]
//
// Exit status: 0 ok, 1 error reply from the daemon, 2 usage/connect error.
func runCtl(args []string) int {
	fs := flag.NewFlagSet("ctl", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	socket := fs.String("socket", "", "control socket path (default: control_socket from -config)")
	configPath := fs.String("config", defaultConfigPath, "config file used to find control_socket")
	fs.Usage = func() {
		fmt.Fprint(os.Stdout, ctlUsageHead)
		fs.PrintDefaults()
		fmt.Fprint(os.Stdout, "\n"+helpText)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0 // -h/--help is a successful request, not an error
		}
		return 2
	}

	// Help never needs the daemon: answer it here, from the same text the
	// daemon would send.
	if rest := fs.Args(); len(rest) == 0 || rest[0] == "help" || wantsHelp(rest) {
		name := ""
		switch {
		case len(rest) > 0 && rest[0] == "help" && len(rest) > 1:
			name = rest[1]
		case len(rest) > 0 && rest[0] != "help":
			name = rest[0]
		}
		h, ok := helpFor(name)
		if !ok {
			fmt.Fprintf(os.Stderr, "ctl: no such command %q\n\n%s", name, helpText)
			return 2
		}
		if name == "" {
			fmt.Fprint(os.Stdout, ctlUsageHead)
			fs.PrintDefaults()
			fmt.Fprint(os.Stdout, "\n")
		}
		fmt.Fprint(os.Stdout, h)
		return 0
	}

	path := *socket
	if path == "" {
		cfg, err := loadConfig(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ctl: cannot read %s to find control_socket (use -socket): %v\n", *configPath, err)
			return 2
		}
		if cfg.ControlSocket == "" {
			fmt.Fprintln(os.Stderr, "ctl: control_socket is empty in the config - the control socket is disabled")
			return 2
		}
		path = cfg.ControlSocket
	}

	conn, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ctl: cannot connect to %s: %v (is the daemon running, and do you have access to the socket?)\n", path, err)
		return 2
	}
	defer conn.Close()
	deadline := controlTimeout + 2*time.Second
	if rest := fs.Args(); len(rest) > 0 && rest[0] == "flush" {
		deadline = flushTotalTimeout + 5*time.Second // a real flush can run for minutes
	}
	_ = conn.SetDeadline(time.Now().Add(deadline))

	line := strings.Join(fs.Args(), " ")
	if _, err := io.WriteString(conn, line+"\n"); err != nil {
		fmt.Fprintf(os.Stderr, "ctl: write: %v\n", err)
		return 2
	}

	reply, err := io.ReadAll(conn)
	if err != nil && len(reply) == 0 {
		fmt.Fprintf(os.Stderr, "ctl: read: %v\n", err)
		return 2
	}
	os.Stdout.Write(reply)
	if strings.HasPrefix(string(reply), "ERROR: ") {
		return 1
	}
	return 0
}
