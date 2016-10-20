#!/bin/bash
set -e

timestamp() {
    until ping -q -c1 localhost > /dev/null 2>&1; do
        sleep 0.5
    done
    date -u +%s > /tmp/boot_timestamp
}
timestamp &

# first check if we're passing flags, if so
# prepend with memcached
if [ "${1:0:1}" = '-' ]; then
	set -- memcached "$@"
fi

exec "$@"
