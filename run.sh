#!/bin/bash
set -e

# This script uses bwrap (bubblewrap) to make the minimum selection of
# paths available for writing to the go commands.
# Modify the config file:
# - Specify GOSDK= for Environment. Must be the directory containing the SDKs.
# - Add script to Run.
# - Set BuildGobin: true.

# We bind to these paths. Normally go invocations create them. But this will
# run also as the first go invocation in this gobuild setup.
if ! test -d "$HOME/go/pkg"; then
	mkdir -p "$HOME/go/pkg"
fi
if ! test -d "$HOME/.cache"; then
	mkdir -p "$HOME/.cache"
fi


# Only "go get -d", "go mod download" and "go list" get access to the network.
net="--unshare-net"
gopkgbind="--ro-bind"
if test "$2" = 'get' -a "$3" = '-d'; then
	gopkgbind="--bind"
	net=""
elif test "$2" = 'mod' -a "$3" = 'download'; then
	gopkgbind="--bind"
	net=""
elif test "$2" = 'list'; then
	gopkgbind="--bind"
	net=""
fi

cachebind="--bind"
# Since go1.19 commands like "go list" also modify the cache. So it can no
# longer be a ro-bind.

gobinbind=""
if test "$GOBUILD_GOBIN" != ""; then
	gobinbind="--bind $GOBUILD_GOBIN $HOME/go/bin"
fi

ulimit -c 0 # max core file size in kb
ulimit -l 0 # max mlock size in kb
ulimit -q 0 # posix message queue size in kb
ulimit -f 256000  # max file size in kb
ulimit -t 1800 # max cpu time in seconds
ulimit -d 4096000 # max data memory segment in kb

exec /usr/bin/bwrap \
	--dev /dev \
	--tmpfs /tmp \
	--proc /proc \
	--ro-bind /etc/resolv.conf /etc/resolv.conf \
	--ro-bind /etc/nsswitch.conf /etc/nsswitch.conf \
	--ro-bind /etc/hosts /etc/hosts \
	--ro-bind /etc/services /etc/services \
	--ro-bind /etc/protocols /etc/protocols \
	--ro-bind /etc/mime.types /etc/mime.types \
	--ro-bind /etc/ssl/certs /etc/ssl/certs \
	--ro-bind /etc/localtime /etc/localtime \
	--ro-bind /usr/share/zoneinfo /usr/share/zoneinfo \
	--ro-bind /usr/bin/nice /usr/bin/nice \
	--ro-bind /lib /lib \
	--ro-bind /lib64 /lib64 \
	--unshare-ipc \
	--unshare-pid \
	--unshare-cgroup \
	--unshare-uts \
	--hostname gobuilds.org \
	--ro-bind $GOSDK $GOSDK \
	--bind $HOME/.cache $HOME/.cache \
	$gopkgbind $HOME/go/pkg $HOME/go/pkg \
	$gobinbind \
	$net \
	/usr/bin/nice \
	-- \
	"$@"
