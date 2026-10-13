PREFIX  ?= /opt/dns-to-bgp
DESTDIR ?=
GO      ?= go
BIN     := bin/unbound-route-state

.PHONY: all build fmt fmt-check vet check-python install clean

all: build

build:
	cd unbound-route-state && CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" \
		-o ../$(BIN) ./cmd/unbound-route-state

fmt:
	gofmt -w unbound-route-state

fmt-check:
	@test -z "$$(gofmt -l unbound-route-state)" || { gofmt -l unbound-route-state; echo "gofmt needed"; exit 1; }

vet:
	cd unbound-route-state && $(GO) vet ./...

check-python:
	python3 -m py_compile unbound-bgp-announcer/*.py
	find unbound-bgp-announcer -name __pycache__ -prune -exec rm -rf {} +

# Installs files only; ownership (root:unbound) is left to the administrator,
# see docs/INSTALL.md. Existing configuration files are never overwritten.
install: build
	install -d $(DESTDIR)$(PREFIX)/unbound-bgp-announcer \
	           $(DESTDIR)$(PREFIX)/unbound-route-state \
	           $(DESTDIR)$(PREFIX)/conf.d $(DESTDIR)$(PREFIX)/domains.d
	install -m 0644 unbound-bgp-announcer/*.py $(DESTDIR)$(PREFIX)/unbound-bgp-announcer/
	install -m 0755 $(BIN) $(DESTDIR)$(PREFIX)/unbound-route-state/unbound-route-state
	for f in state communities announcer; do \
	    [ -e $(DESTDIR)$(PREFIX)/conf.d/$$f.conf ] || \
	        install -m 0640 examples/conf.d/$$f.conf $(DESTDIR)$(PREFIX)/conf.d/$$f.conf; \
	done
	for f in examples/domains.d/*.list; do \
	    [ -e $(DESTDIR)$(PREFIX)/domains.d/$$(basename $$f) ] || \
	        install -m 0644 $$f $(DESTDIR)$(PREFIX)/domains.d/; \
	done
	@echo "systemd unit: examples/systemd/unbound-route-state.service -> /etc/systemd/system/"

clean:
	rm -rf bin
