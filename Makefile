PREFIX ?= /usr/local/bin

.PHONY: build install uninstall clean test

build:
	go build -o ddl ./cmd/ddl
	go build -o ddld ./cmd/ddld
	go build -o ddl-guest ./cmd/ddl-guest

install: build
	install -m 755 ddl $(PREFIX)/ddl
	install -m 755 ddld $(PREFIX)/ddld
	install -m 755 ddl-guest $(PREFIX)/ddl-guest

uninstall:
	rm -f $(PREFIX)/ddl $(PREFIX)/ddld $(PREFIX)/ddl-guest

clean:
	rm -f ddl ddld ddl-guest

test:
	go test ./...
