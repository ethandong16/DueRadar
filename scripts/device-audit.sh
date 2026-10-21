#!/bin/sh
# Read-only, credential-free Linux inventory. Do not collect hostnames or IPs.
set -u

section() { printf '\n[%s]\n' "$1"; }

section timestamp
date -u '+%Y-%m-%dT%H:%M:%SZ'

section architecture
uname -m
getconf LONG_BIT
uname -r
getconf _NPROCESSORS_ONLN
if [ -r /proc/device-tree/model ]; then
    tr '\000' '\n' < /proc/device-tree/model
fi
if [ -r /proc/device-tree/compatible ]; then
    tr '\000' '\n' < /proc/device-tree/compatible
fi
sed -n '/^CPU implementer/p; /^CPU architecture/p; /^CPU part/p; /^model name/p' /proc/cpuinfo

section operating_system
sed -n '/^PRETTY_NAME=/p; /^VERSION_ID=/p' /etc/os-release

section memory_kib
free -k
cat /proc/swaps

section root_storage_kib
df -k -T /
if command -v lsblk >/dev/null 2>&1; then
    lsblk -o NAME,SIZE,TYPE,FSTYPE
fi

section init
ps -p 1 -o comm=
if command -v systemctl >/dev/null 2>&1; then
    systemctl --version | head -n 1
    systemctl is-system-running || true
    # Instance names can include addresses, users or application arguments.
    systemctl list-units --state=failed --no-pager --no-legend --plain 2>/dev/null |
        awk '{print $1}' | sed 's/@.*\.service$/@instance.service/'
fi

section docker
if command -v docker >/dev/null 2>&1; then
    docker version --format 'client={{.Client.Version}} server={{.Server.Version}}' 2>/dev/null || true
else
    printf 'docker=not_installed\n'
fi
if [ -e /var/run/docker.sock ]; then
    printf 'docker_socket=present\n'
else
    printf 'docker_socket=absent\n'
fi
if [ -r /sys/fs/cgroup/cgroup.controllers ]; then
    printf 'cgroup_v2_controllers='
    cat /sys/fs/cgroup/cgroup.controllers
fi
if [ -r /proc/config.gz ]; then
    zcat /proc/config.gz | sed -n '/^CONFIG_NAMESPACES=/p; /^CONFIG_CGROUPS=/p; /^CONFIG_OVERLAY_FS=/p'
fi

section tailscale
if command -v tailscale >/dev/null 2>&1; then
    tailscale version 2>/dev/null | head -n 1
else
    printf 'tailscale=not_installed\n'
fi
if command -v systemctl >/dev/null 2>&1; then
    systemctl is-active tailscaled 2>/dev/null || true
fi

section running_services
if command -v systemctl >/dev/null 2>&1; then
    systemctl list-units --type=service --state=running --no-pager --no-legend --plain 2>/dev/null |
        awk '{print $1}' | sed 's/@.*\.service$/@instance.service/'
fi

section clock
if command -v timedatectl >/dev/null 2>&1; then
    timedatectl show -p NTPSynchronized -p Timezone 2>/dev/null || true
fi

section load
awk '{print "load1=" $1 " load5=" $2 " load15=" $3}' /proc/loadavg
awk '{print "uptime_seconds=" $1}' /proc/uptime
