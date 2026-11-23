package mcp

import "strings"

// PlaywrightPreset returns a ServerEndpoint for Microsoft's maintained @playwright/mcp server.
// When display is non-empty (e.g. ":99" inside soma desktop), it launches headed; otherwise headless.
func PlaywrightPreset(display string, isolatedDesktop bool) ServerEndpoint {
	args := []string{"-y", "@playwright/mcp@latest"}
	env := map[string]string{}
	if strings.TrimSpace(display) == "" {
		args = append(args, "--headless")
	} else {
		env["DISPLAY"] = display
	}
	return ServerEndpoint{
		Name:            "playwright",
		Transport:       "stdio",
		Command:         "npx",
		Args:            args,
		Env:             env,
		IsolatedDesktop: isolatedDesktop,
	}
}

// ChromeDevToolsPreset returns a ServerEndpoint for chrome-devtools-mcp (CDP debugging, console, network).
func ChromeDevToolsPreset(autoConnect bool) ServerEndpoint {
	args := []string{"-y", "chrome-devtools-mcp@latest"}
	if autoConnect {
		args = append(args, "--autoConnect")
	}
	return ServerEndpoint{
		Name:      "chrome-devtools",
		Transport: "stdio",
		Command:   "npx",
		Args:      args,
	}
}

// CUADriverPreset returns a ServerEndpoint for trycua/cua's maintained Rust cua-driver MCP server
// (non-intrusive background AT-SPI/AX/UIAutomation control).
func CUADriverPreset(isolatedDesktop bool) ServerEndpoint {
	return ServerEndpoint{
		Name:            "cua-desktop",
		Transport:       "stdio",
		Command:         "cua-driver",
		Args:            []string{"mcp"},
		IsolatedDesktop: isolatedDesktop,
	}
}

// DockerDesktopPreset returns a stdio MCP ServerEndpoint that connects into a running soma desktop
// Docker container via `docker exec -i <container> python3 /opt/soma-desktop/desktop_mcp.py`.
func DockerDesktopPreset(containerName string) ServerEndpoint {
	return ServerEndpoint{
		Name:            "desktop",
		Transport:       "stdio",
		Command:         "docker",
		Args:            []string{"exec", "-i", containerName, "python3", "/opt/soma-desktop/desktop_mcp.py"},
		IsolatedDesktop: true,
	}
}

// DockerPlaywrightPreset connects to @playwright/mcp running headed on DISPLAY=:99 inside the desktop container.
func DockerPlaywrightPreset(containerName string) ServerEndpoint {
	return ServerEndpoint{
		Name:            "playwright",
		Transport:       "stdio",
		Command:         "docker",
		Args:            []string{"exec", "-i", "-e", "DISPLAY=:99", containerName, "npx", "-y", "@playwright/mcp@latest"},
		IsolatedDesktop: true,
	}
}
