.DEFAULT_GOAL := release
.DELETE_ON_ERROR:

GO ?= go
SHA256SUM ?= sha256sum
GOCACHE ?= /tmp/phicomm-go-cache
export GOCACHE

SOURCES := phicomm-s7d.go favicon.svg Makefile
RELEASE_FILES := dist/phicomm-s7d-linux-arm64 dist/phicomm-s7d.init dist/openwrt-firewall-setup.sh
RELEASE_CHECKSUMS := $(addsuffix .sha256sum,$(RELEASE_FILES))

.PHONY: all build arm64 release checksums test

all: release

build: phicomm-s7d

arm64: phicomm-s7d-linux-arm64

release: $(RELEASE_FILES) $(RELEASE_CHECKSUMS)

checksums: $(RELEASE_CHECKSUMS)

test: build
	./phicomm-s7d --self-test

phicomm-s7d: $(SOURCES)
	$(GO) build -o "$@" phicomm-s7d.go

phicomm-s7d-linux-arm64: $(SOURCES)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build \
		-trimpath -gcflags='all=-l' -ldflags='-s -w -buildid=' \
		-o "$@" phicomm-s7d.go

dist:
	mkdir -p "$@"

dist/phicomm-s7d-linux-arm64: phicomm-s7d-linux-arm64 | dist
	cp "$<" "$@"
	chmod 0755 "$@"

dist/phicomm-s7d.init: init.d/phicomm-s7d | dist
	cp "$<" "$@"
	chmod 0755 "$@"

dist/openwrt-firewall-setup.sh: openwrt-firewall-setup.sh | dist
	cp "$<" "$@"
	chmod 0755 "$@"

dist/%.sha256sum: dist/%
	cd dist && $(SHA256SUM) "$(notdir $<)" > "$(notdir $@)"
