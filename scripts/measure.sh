#!/bin/sh
# Usage: sh scripts/measure.sh PID [samples=30] [interval_seconds=2]
# CPU percentage uses one full CPU core as 100%; RSS/HWM come from /proc.
set -eu

pid=${1:-}
samples=${2:-30}
interval=${3:-2}
for value in "$pid" "$samples" "$interval"; do
    case "$value" in
        ''|*[!0-9]*|0) printf 'PID, samples and interval must be positive integers.\n' >&2; exit 2 ;;
    esac
done
if [ ! -r "/proc/$pid/stat" ]; then
    printf 'Process is not readable.\n' >&2
    exit 3
fi

process_ticks() {
    awk '{sub(/^.*\) /, ""); print $12 + $13}' "/proc/$pid/stat"
}
system_ticks() {
    awk '/^cpu / {for (i=2; i<=9; i++) total += $i; print total; exit}' /proc/stat
}
process_start() {
    awk '{sub(/^.*\) /, ""); print $20}' "/proc/$pid/stat"
}

cores=$(getconf _NPROCESSORS_ONLN)
identity=$(process_start)
previous_process=$(process_ticks)
previous_system=$(system_ticks)
started=$(awk '{print $1}' /proc/uptime)
printf 'elapsed_seconds,rss_kib,hwm_kib,cpu_percent_one_core,system_available_kib,system_load1\n'
index=0
while [ "$index" -lt "$samples" ]; do
    sleep "$interval"
    if [ ! -r "/proc/$pid/stat" ] || [ "$(process_start)" != "$identity" ]; then
        printf 'Measured process exited or PID was reused.\n' >&2
        exit 3
    fi
    current_process=$(process_ticks)
    current_system=$(system_ticks)
    elapsed=$(awk -v start="$started" '{printf "%.2f", $1-start}' /proc/uptime)
    rss=$(awk '/^VmRSS:/ {print $2; found=1} END {if (!found) print 0}' "/proc/$pid/status")
    hwm=$(awk '/^VmHWM:/ {print $2; found=1} END {if (!found) print 0}' "/proc/$pid/status")
    cpu=$(awk -v p="$current_process" -v pp="$previous_process" -v s="$current_system" -v ps="$previous_system" -v n="$cores" 'BEGIN {if (s>ps) printf "%.2f", 100*n*(p-pp)/(s-ps); else printf "0.00"}')
    available=$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo)
    load=$(awk '{print $1}' /proc/loadavg)
    printf '%s,%s,%s,%s,%s,%s\n' "$elapsed" "$rss" "$hwm" "$cpu" "$available" "$load"
    previous_process=$current_process
    previous_system=$current_system
    index=$((index+1))
done
