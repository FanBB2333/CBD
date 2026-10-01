#!/bin/sh
# Read-only inventory. Does not open AT ports, load drivers, or start containers.
set -u
section() { printf '\n[%s]\n' "$1"; }

section 'Host'
uname -srm
if command -v getcfg >/dev/null 2>&1; then
    getcfg System Model -f /etc/config/uLinux.conf
    getcfg System Version -f /etc/config/uLinux.conf
    getcfg System 'Build Number' -f /etc/config/uLinux.conf
fi
if [ -r /proc/meminfo ]; then
    awk '/^MemTotal:|^MemAvailable:/ { print }' /proc/meminfo
fi
if [ -n "${1:-}" ]; then
    section 'Requested data volume'
    df -h "$1"
fi

section 'Container engine'
docker_bin=$(command -v docker 2>/dev/null || true)
if [ -z "$docker_bin" ] && command -v getcfg >/dev/null 2>&1; then
    cs_path=$(getcfg container-station Install_Path -f /etc/config/qpkg.conf)
    if [ -x "$cs_path/bin/docker" ]; then docker_bin="$cs_path/bin/docker"; fi
fi
if [ -n "$docker_bin" ]; then
    "$docker_bin" version --format 'Client={{.Client.Version}} Server={{.Server.Version}}'
    "$docker_bin" info --format 'Arch={{.Architecture}} Driver={{.Driver}} Cgroup={{.CgroupDriver}} Root={{.DockerRootDir}}'
    "$docker_bin" compose version
    "$docker_bin" ps --format 'Name={{.Names}} Status={{.Status}} Ports={{.Ports}}'
else
    printf '%s\n' 'Docker CLI not found; check Container Station installation/PATH.'
fi

section 'USB and serial devices'
if command -v lsusb >/dev/null 2>&1; then lsusb; fi
for device in /dev/ttyUSB* /dev/ttyACM*; do
    [ -c "$device" ] || continue
    ls -l "$device"
    readlink -f "/sys/class/tty/${device##*/}/device"
done
if [ -d /dev/bus/usb ]; then ls -ld /dev/bus/usb; fi

section 'ALSA capture/playback'
if [ -r /proc/asound/cards ]; then cat /proc/asound/cards; else printf '%s\n' 'No host ALSA cards detected.'; fi
if [ -d /dev/snd ]; then ls -l /dev/snd; fi
if command -v arecord >/dev/null 2>&1; then arecord -l; fi
if command -v aplay >/dev/null 2>&1; then aplay -l; fi
if [ -r /proc/modules ]; then
    awk '$1 ~ /^(snd_usb_audio|snd_usbmidi_lib|usbserial|option|cdc_acm)$/ { print }' /proc/modules
fi
printf '\n%s\n' 'Inventory only: USB identity, AT response, ADB ABI and real call audio still need validation.'
