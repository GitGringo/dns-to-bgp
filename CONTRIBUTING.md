# Contributing

Bug reports, documentation fixes and pull requests are welcome.

## Before you open a pull request

```bash
make fmt-check vet build check-python
```

- Go code must be `gofmt`-clean and pass `go vet`.
- Keep the Python module dependency-free: it runs inside Unbound and must use only the
  standard library.
- Keep behaviour changes and documentation in the same pull request: update
  `docs/` and `examples/` when a configuration key or a command changes.
- Code comments, log messages and documentation are written in English.
- Never include real addresses, AS numbers, hostnames or domain lists from your
  own network in code, docs, examples or issues. Use RFC 5737 / RFC 3849 /
  RFC 6996 documentation values (`192.0.2.0/24`, `2001:db8::/32`, AS `64512`).

## Layout

| Path | Contents |
|---|---|
| `unbound-bgp-announcer/` | Python module loaded by Unbound |
| `unbound-route-state/` | Go daemon (`cmd/`, `internal/state`, `internal/bgp`) |
| `examples/` | Example configuration for every component |
| `docs/` | Installation guides, reference and internals |

By contributing you agree that your contribution is licensed under the MIT License.
