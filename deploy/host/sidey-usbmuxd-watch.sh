#!/bin/bash
# Restart usbmuxd if an Apple device is attached at USB level but invisible
# to usbmuxd. Over a VirtualHere/Tailscale link (D13) usbmuxd drops devices
# on "RX transfer stalled" and only re-enumerates on a udev event, which
# never comes for a virtual USB port. Pairing records persist in
# /var/lib/lockdown, so a restart is safe and re-attaches instantly.
set -u

lsusb | grep -qi apple || exit 0

if idevice_id -l 2>/dev/null | grep -q .; then
  exit 0
fi

logger -t sidey-usbmuxd-watch "iPhone attached but not listed by usbmuxd; restarting usbmuxd"
systemctl restart usbmuxd
