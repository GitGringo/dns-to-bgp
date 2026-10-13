package bgp

// SessionInfo is a point-in-time view of the single BGP session, for the
// daemon's status output (ctl stats / ctl bgp). Kept free of any GoBGP type
// so callers (and tests) never need to import GoBGP.
type SessionInfo struct {
	State       string // lower-case FSM state: "established", "active", "idle", ...
	Established bool

	// UptimeSeconds is time since the session last reached Established
	// (0 if it is not established right now).
	UptimeSeconds int64

	// Flaps counts transitions out of Established since the daemon started.
	Flaps uint32

	// DisconnectReason/Message describe the most recent session teardown
	// ("" / "" if none has happened yet).
	DisconnectReason  string
	DisconnectMessage string

	UpdatesSent           uint64
	UpdatesReceived       uint64
	WithdrawsSent         uint64 // UPDATE messages carrying withdrawals
	NotificationsSent     uint64
	NotificationsReceived uint64
}
