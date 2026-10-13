# Security policy

Please report vulnerabilities privately through GitHub's
"Report a vulnerability" button on the repository's **Security** tab instead of opening a
public issue.

Hardening notes for operators:

- The daemon needs no privileges except, optionally, `CAP_NET_ADMIN` for the conntrack
  keepalive; the provided systemd unit grants only that.
- The data and control sockets are created with mode `0660`; anyone in the `unbound`
  group can send events or query the control socket. Keep the group small.
- The control socket is read-only except for `ctl flush`, which runs `unbound-control`.
