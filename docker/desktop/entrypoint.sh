#!/usr/bin/env bash
set -euo pipefail

export DISPLAY="${DISPLAY:-:99}"
W="${SCREEN_WIDTH:-1280}"
H="${SCREEN_HEIGHT:-800}"
D="${SCREEN_DEPTH:-24}"
VNC_PASS="${SOMA_VNC_PASSWORD:-soma}"

# Start virtual X11 display.
Xvfb "${DISPLAY}" -screen 0 "${W}x${H}x${D}" -ac +extension GLX +render -noreset >/tmp/xvfb.log 2>&1 &
for _ in $(seq 1 40); do
  if xdpyinfo -display "${DISPLAY}" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done

# Start D-Bus + AT-SPI2 + XFCE desktop session.
eval "$(dbus-launch --sh-syntax)"
export DBUS_SESSION_BUS_ADDRESS
/usr/libexec/at-spi-bus-launcher --launch-immediately >/tmp/atspi.log 2>&1 &
startxfce4 >/tmp/xfce.log 2>&1 &
sleep 1

# Configure VNC password and start x11vnc + noVNC websockify on port 6080.
x11vnc -storepasswd "${VNC_PASS}" /home/soma/.vnc/passwd >/dev/null 2>&1
x11vnc -display "${DISPLAY}" -rfbauth /home/soma/.vnc/passwd -rfbport 5900 -forever -shared -bg -o /tmp/x11vnc.log
websockify --web=/usr/share/novnc/ 6080 127.0.0.1:5900 >/tmp/novnc.log 2>&1 &

echo "soma-desktop ready on DISPLAY=${DISPLAY} (${W}x${H}) noVNC=6080"
wait
