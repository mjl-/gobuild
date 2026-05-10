#!/usr/bin/env bash
set -e

# This script uses bwrap (bubblewrap) to make the minimum selection of
# paths available for writing to the go commands.
# Modify the config file:
# - Specify GOSDK= for Environment. Must be the directory containing the SDKs.
# - Add script to Run.

ulimit -c 0 # max core file size in kb
ulimit -l 0 # max mlock size in kb
ulimit -q 0 # posix message queue size in kb
ulimit -f 256000  # max file size in kb
ulimit -t 1800 # max cpu time in seconds
ulimit -d 4096000 # max data memory segment in kb

exec setpriv --ambient-caps -all \
	bwrap \
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
	--ro-bind /etc/pki /etc/pki \
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
	--bind $HOME $HOME \
	/usr/bin/nice \
	-- \
	"$@"
