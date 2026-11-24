FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive \
    DISPLAY=:99 \
    SCREEN_WIDTH=1280 \
    SCREEN_HEIGHT=800 \
    SCREEN_DEPTH=24 \
    GTK_MODULES=gail:atk-bridge \
    QT_ACCESSIBILITY=1 \
    NO_AT_BRIDGE=0

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    dbus-x11 \
    at-spi2-core \
    python3 \
    python3-pil \
    python3-pyatspi \
    xfce4 \
    xfce4-terminal \
    xvfb \
    x11vnc \
    novnc \
    websockify \
    xdotool \
    wmctrl \
    scrot \
    xclip \
    chromium \
    fonts-dejavu-core \
    fonts-liberation \
    git \
    nodejs \
    npm \
    python3-pip \
    python3-venv \
    sudo \
    unzip \
    wget \
    xdg-utils \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -ms /bin/bash -u 1000 soma && \
    echo "soma ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/soma && \
    chmod 0440 /etc/sudoers.d/soma && \
    mkdir -p /opt/soma-desktop /workspace /home/soma/.vnc && \
    chown -R soma:soma /opt/soma-desktop /workspace /home/soma

COPY entrypoint.sh /opt/soma-desktop/entrypoint.sh
COPY desktop_mcp.py /opt/soma-desktop/desktop_mcp.py
RUN chmod +x /opt/soma-desktop/entrypoint.sh /opt/soma-desktop/desktop_mcp.py

USER soma
WORKDIR /workspace

EXPOSE 6080

ENTRYPOINT ["/opt/soma-desktop/entrypoint.sh"]
