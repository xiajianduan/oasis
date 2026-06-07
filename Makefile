.PHONY: build clean install run start stop restart tun2socks-download

BINARY=oasis
MAIN=./cmd/oasis
PREFIX=/usr/local
LIBEXEC=$(DESTDIR)$(PREFIX)/libexec

TUN2SOCKS_VERSION = 2.6.0
TUN2SOCKS_ASSET = internal/assets/tun2socks_darwin_amd64

$(TUN2SOCKS_ASSET):
	curl -sSfL -o /tmp/tun2socks.zip "https://github.com/xjasonlyu/tun2socks/releases/download/v$(TUN2SOCKS_VERSION)/tun2socks-darwin-amd64.zip"
	unzip -p /tmp/tun2socks.zip tun2socks-darwin-amd64 > $@
	rm -f /tmp/tun2socks.zip
	chmod +x $@

build: $(TUN2SOCKS_ASSET)
	go build -o $(BINARY) $(MAIN)

install: build
	@echo "==> 安装中（需要管理员密码一次）..."
	osascript -e 'do shell script "\
		mkdir -p $(PREFIX)/bin $(PREFIX)/libexec && \
		cp -f $(CURDIR)/$(BINARY) $(PREFIX)/bin/$(BINARY) && \
		cp -f $(CURDIR)/$(TUN2SOCKS_ASSET) $(PREFIX)/libexec/oasis-tun2socks && \
		printf \"%s\\n\" \"ALL ALL=(ALL) NOPASSWD: /sbin/ifconfig, /sbin/route, /usr/bin/pkill, /usr/sbin/networksetup, /sbin/pfctl, $(PREFIX)/libexec/oasis-tun2socks\" > /etc/sudoers.d/oasis && \
		chmod 440 /etc/sudoers.d/oasis \
	" with administrator privileges'
	@echo "==> 安装完成，运行 oasis start 启动"

clean:
	go clean
	rm -f $(BINARY)

run: build
	./$(BINARY) start

start: build
	./$(BINARY) start

stop:
	./$(BINARY) stop

restart: build
	./$(BINARY) stop
	./$(BINARY) start
