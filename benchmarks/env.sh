#!/usr/bin/env bash
# Print the environment a benchmark ran in, to store next to its results.
echo "date: $(date -u +%FT%TZ)"
echo "kernel: $(uname -r) $(uname -m)"
echo "os: $(. /etc/os-release && echo "$PRETTY_NAME")"
echo "cpus: $(nproc)"
echo "memory: $(awk '/MemTotal/ {printf "%.1f GiB", $2 / 1048576}' /proc/meminfo)"
echo "virtualization: $(systemd-detect-virt 2>/dev/null || echo unknown)"
echo "cpu: $(lscpu | awk -F: '/Vendor ID|Model name/ {gsub(/^ +/, "", $2); printf "%s ", $2}')"
echo "page size: $(getconf PAGESIZE)"
echo "tracelens commit: $(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
