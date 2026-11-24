package mcp

import (
	"fmt"
	"sort"
	"strings"
)

// PresetInfo describes a curated MCP connector preset.
type PresetInfo struct {
	Name        string
	Category    string
	Description string
	RequiredEnv []string
	Build       func(args []string) ServerEndpoint
}

func npxPreset(name, pkg string, envVars ...string) func([]string) ServerEndpoint {
	return func(args []string) ServerEndpoint {
		cmdArgs := append([]string{"-y", pkg}, args...)
		var env map[string]string
		if len(envVars) > 0 {
			env = make(map[string]string, len(envVars))
			for _, k := range envVars {
				env[k] = "${" + k + "}"
			}
		}
		return ServerEndpoint{
			Name:      name,
			Transport: "stdio",
			Command:   "npx",
			Args:      cmdArgs,
			Env:       env,
		}
	}
}

func uvxPreset(name, pkg string, envVars ...string) func([]string) ServerEndpoint {
	return func(args []string) ServerEndpoint {
		cmdArgs := append([]string{pkg}, args...)
		var env map[string]string
		if len(envVars) > 0 {
			env = make(map[string]string, len(envVars))
			for _, k := range envVars {
				env[k] = "${" + k + "}"
			}
		}
		return ServerEndpoint{
			Name:      name,
			Transport: "stdio",
			Command:   "uvx",
			Args:      cmdArgs,
			Env:       env,
		}
	}
}

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

var presetCatalog = map[string]PresetInfo{
	// Developer & Code
	"github": {
		Name:        "github",
		Category:    "Developer & Code",
		Description: "GitHub repositories, issues, PRs, and code search (@modelcontextprotocol/server-github)",
		RequiredEnv: []string{"GITHUB_PERSONAL_ACCESS_TOKEN"},
		Build:       npxPreset("github", "@modelcontextprotocol/server-github", "GITHUB_PERSONAL_ACCESS_TOKEN"),
	},
	"gitlab": {
		Name:        "gitlab",
		Category:    "Developer & Code",
		Description: "GitLab merge requests, pipelines, and issues (@modelcontextprotocol/server-gitlab)",
		RequiredEnv: []string{"GITLAB_PERSONAL_ACCESS_TOKEN"},
		Build:       npxPreset("gitlab", "@modelcontextprotocol/server-gitlab", "GITLAB_PERSONAL_ACCESS_TOKEN"),
	},
	"filesystem": {
		Name:        "filesystem",
		Category:    "Developer & Code",
		Description: "Scoped local filesystem access (@modelcontextprotocol/server-filesystem <dir>)",
		Build: func(args []string) ServerEndpoint {
			dir := "."
			if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
				dir = args[0]
			}
			return ServerEndpoint{
				Name:      "filesystem",
				Transport: "stdio",
				Command:   "npx",
				Args:      []string{"-y", "@modelcontextprotocol/server-filesystem", dir},
			}
		},
	},
	"git": {
		Name:        "git",
		Category:    "Developer & Code",
		Description: "Local Git repository status, diffs, logs, and commits (mcp-server-git)",
		Build:       uvxPreset("git", "mcp-server-git"),
	},
	"sentry": {
		Name:        "sentry",
		Category:    "Developer & Code",
		Description: "Sentry error tracking, stack traces, and issue triage (mcp-server-sentry)",
		RequiredEnv: []string{"SENTRY_AUTH_TOKEN"},
		Build:       uvxPreset("sentry", "mcp-server-sentry", "SENTRY_AUTH_TOKEN"),
	},
	"docker": {
		Name:        "docker",
		Category:    "Developer & Code",
		Description: "Manage local Docker containers, images, and logs (mcp-server-docker)",
		Build:       uvxPreset("docker", "mcp-server-docker"),
	},
	"kubernetes": {
		Name:        "kubernetes",
		Category:    "Developer & Code",
		Description: "Inspect and manage Kubernetes pods, deployments, and logs (mcp-server-kubernetes)",
		Build:       npxPreset("kubernetes", "mcp-server-kubernetes"),
	},

	// Cloud & Infra
	"cloudflare": {
		Name:        "cloudflare",
		Category:    "Cloud & Infra",
		Description: "Cloudflare Workers, KV, D1, R2, and DNS (@cloudflare/mcp-server-cloudflare)",
		RequiredEnv: []string{"CLOUDFLARE_API_TOKEN"},
		Build:       npxPreset("cloudflare", "@cloudflare/mcp-server-cloudflare", "CLOUDFLARE_API_TOKEN"),
	},
	"vercel": {
		Name:        "vercel",
		Category:    "Cloud & Infra",
		Description: "Vercel deployments, build logs, projects, and domains (@vercel/mcp-server)",
		RequiredEnv: []string{"VERCEL_API_TOKEN"},
		Build:       npxPreset("vercel", "@vercel/mcp-server", "VERCEL_API_TOKEN"),
	},
	"supabase": {
		Name:        "supabase",
		Category:    "Cloud & Infra",
		Description: "Supabase SQL, migrations, edge functions, and auth (@supabase/mcp-server-supabase)",
		RequiredEnv: []string{"SUPABASE_ACCESS_TOKEN"},
		Build:       npxPreset("supabase", "@supabase/mcp-server-supabase", "SUPABASE_ACCESS_TOKEN"),
	},
	"netlify": {
		Name:        "netlify",
		Category:    "Cloud & Infra",
		Description: "Netlify sites, deploys, functions, and environment variables (@netlify/mcp)",
		RequiredEnv: []string{"NETLIFY_PAT"},
		Build:       npxPreset("netlify", "@netlify/mcp", "NETLIFY_PAT"),
	},
	"aws-docs": {
		Name:        "aws-docs",
		Category:    "Cloud & Infra",
		Description: "Search and read official AWS documentation (awslabs.aws-documentation-mcp-server)",
		Build:       uvxPreset("aws-docs", "awslabs.aws-documentation-mcp-server"),
	},

	// Databases
	"sqlite": {
		Name:        "sqlite",
		Category:    "Databases",
		Description: "SQLite database query & schema inspection (mcp-server-sqlite <db-path>)",
		Build: func(args []string) ServerEndpoint {
			dbPath := "sqlite.db"
			if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
				dbPath = args[0]
			}
			return ServerEndpoint{
				Name:      "sqlite",
				Transport: "stdio",
				Command:   "uvx",
				Args:      []string{"mcp-server-sqlite", "--db-path", dbPath},
			}
		},
	},
	"postgres": {
		Name:        "postgres",
		Category:    "Databases",
		Description: "Read-only PostgreSQL schema & query access (@modelcontextprotocol/server-postgres <dsn>)",
		Build: func(args []string) ServerEndpoint {
			dsn := "${POSTGRES_URL}"
			if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
				dsn = args[0]
			}
			return ServerEndpoint{
				Name:      "postgres",
				Transport: "stdio",
				Command:   "npx",
				Args:      []string{"-y", "@modelcontextprotocol/server-postgres", dsn},
			}
		},
	},
	"redis": {
		Name:        "redis",
		Category:    "Databases",
		Description: "Redis key-value inspection and commands (@modelcontextprotocol/server-redis)",
		RequiredEnv: []string{"REDIS_URL"},
		Build:       npxPreset("redis", "@modelcontextprotocol/server-redis", "REDIS_URL"),
	},
	"mongodb": {
		Name:        "mongodb",
		Category:    "Databases",
		Description: "MongoDB collections, aggregations, and schema inspection (mongodb-mcp-server)",
		RequiredEnv: []string{"MDB_MCP_CONNECTION_STRING"},
		Build:       npxPreset("mongodb", "mongodb-mcp-server", "MDB_MCP_CONNECTION_STRING"),
	},
	"mysql": {
		Name:        "mysql",
		Category:    "Databases",
		Description: "MySQL schema inspection and SQL queries (@benborla29/mcp-server-mysql)",
		RequiredEnv: []string{"MYSQL_HOST", "MYSQL_USER", "MYSQL_PASS", "MYSQL_DB"},
		Build:       npxPreset("mysql", "@benborla29/mcp-server-mysql", "MYSQL_HOST", "MYSQL_USER", "MYSQL_PASS", "MYSQL_DB"),
	},

	// Productivity & Workspace
	"google-drive": {
		Name:        "google-drive",
		Category:    "Productivity",
		Description: "Google Drive file search, reading, and workspace indexing (@modelcontextprotocol/server-gdrive)",
		RequiredEnv: []string{"GDRIVE_CREDENTIALS_PATH"},
		Build:       npxPreset("google-drive", "@modelcontextprotocol/server-gdrive", "GDRIVE_CREDENTIALS_PATH"),
	},
	"google-calendar": {
		Name:        "google-calendar",
		Category:    "Productivity",
		Description: "Google Calendar events, scheduling, and availability (@cocal/google-calendar-mcp)",
		RequiredEnv: []string{"GOOGLE_OAUTH_CREDENTIALS"},
		Build:       npxPreset("google-calendar", "@cocal/google-calendar-mcp", "GOOGLE_OAUTH_CREDENTIALS"),
	},
	"gmail": {
		Name:        "gmail",
		Category:    "Productivity",
		Description: "Gmail search, reading, drafting, and labeling (@gongrzhe/server-gmail-autoauth-mcp)",
		Build:       npxPreset("gmail", "@gongrzhe/server-gmail-autoauth-mcp"),
	},
	"jira": {
		Name:        "jira",
		Category:    "Productivity",
		Description: "Atlassian Jira issues, sprints, and transitions (mcp-atlassian)",
		RequiredEnv: []string{"JIRA_URL", "JIRA_USERNAME", "JIRA_API_TOKEN"},
		Build:       uvxPreset("jira", "mcp-atlassian", "JIRA_URL", "JIRA_USERNAME", "JIRA_API_TOKEN"),
	},
	"confluence": {
		Name:        "confluence",
		Category:    "Productivity",
		Description: "Atlassian Confluence pages, spaces, and CQL search (mcp-atlassian)",
		RequiredEnv: []string{"CONFLUENCE_URL", "CONFLUENCE_USERNAME", "CONFLUENCE_API_TOKEN"},
		Build:       uvxPreset("confluence", "mcp-atlassian", "CONFLUENCE_URL", "CONFLUENCE_USERNAME", "CONFLUENCE_API_TOKEN"),
	},
	"linear": {
		Name:        "linear",
		Category:    "Productivity",
		Description: "Linear issues, projects, and cycles via Streamable HTTP + OAuth 2.1",
		Build: func(_ []string) ServerEndpoint {
			return ServerEndpoint{
				Name:      "linear",
				Transport: "streamable_http",
				URL:       "https://mcp.linear.app/mcp",
				OAuth:     &OAuthConfig{Enabled: true},
			}
		},
	},
	"notion": {
		Name:        "notion",
		Category:    "Productivity",
		Description: "Notion pages, databases, and search (@notionhq/notion-mcp-server)",
		RequiredEnv: []string{"NOTION_TOKEN"},
		Build:       npxPreset("notion", "@notionhq/notion-mcp-server", "NOTION_TOKEN"),
	},
	"asana": {
		Name:        "asana",
		Category:    "Productivity",
		Description: "Asana tasks, projects, and team workflows (@roychri/mcp-server-asana)",
		RequiredEnv: []string{"ASANA_ACCESS_TOKEN"},
		Build:       npxPreset("asana", "@roychri/mcp-server-asana", "ASANA_ACCESS_TOKEN"),
	},
	"todoist": {
		Name:        "todoist",
		Category:    "Productivity",
		Description: "Todoist tasks, projects, and due dates (@abhiz123/todoist-mcp-server)",
		RequiredEnv: []string{"TODOIST_API_TOKEN"},
		Build:       npxPreset("todoist", "@abhiz123/todoist-mcp-server", "TODOIST_API_TOKEN"),
	},
	"obsidian": {
		Name:        "obsidian",
		Category:    "Productivity",
		Description: "Obsidian vault notes, links, and search (mcp-obsidian)",
		RequiredEnv: []string{"OBSIDIAN_API_KEY"},
		Build:       uvxPreset("obsidian", "mcp-obsidian", "OBSIDIAN_API_KEY"),
	},
	"airtable": {
		Name:        "airtable",
		Category:    "Productivity",
		Description: "Airtable bases, tables, and records (airtable-mcp-server)",
		RequiredEnv: []string{"AIRTABLE_API_KEY"},
		Build:       npxPreset("airtable", "airtable-mcp-server", "AIRTABLE_API_KEY"),
	},

	// Communication & Chat
	"slack": {
		Name:        "slack",
		Category:    "Communication",
		Description: "Slack channels, messages, and threads (@modelcontextprotocol/server-slack)",
		RequiredEnv: []string{"SLACK_BOT_TOKEN", "SLACK_TEAM_ID"},
		Build:       npxPreset("slack", "@modelcontextprotocol/server-slack", "SLACK_BOT_TOKEN", "SLACK_TEAM_ID"),
	},
	"discord": {
		Name:        "discord",
		Category:    "Communication",
		Description: "Discord guilds, channels, and messages (mcp-discord)",
		RequiredEnv: []string{"DISCORD_TOKEN"},
		Build:       uvxPreset("discord", "mcp-discord", "DISCORD_TOKEN"),
	},

	// Web & Search
	"brave-search": {
		Name:        "brave-search",
		Category:    "Web & Search",
		Description: "Web & local search via Brave Search API (@modelcontextprotocol/server-brave-search)",
		RequiredEnv: []string{"BRAVE_API_KEY"},
		Build:       npxPreset("brave-search", "@modelcontextprotocol/server-brave-search", "BRAVE_API_KEY"),
	},
	"fetch": {
		Name:        "fetch",
		Category:    "Web & Search",
		Description: "Web page fetching & Markdown extraction (mcp-server-fetch)",
		Build:       uvxPreset("fetch", "mcp-server-fetch"),
	},
	"firecrawl": {
		Name:        "firecrawl",
		Category:    "Web & Search",
		Description: "Deep web crawling & clean Markdown extraction (firecrawl-mcp)",
		RequiredEnv: []string{"FIRECRAWL_API_KEY"},
		Build:       npxPreset("firecrawl", "firecrawl-mcp", "FIRECRAWL_API_KEY"),
	},
	"exa": {
		Name:        "exa",
		Category:    "Web & Search",
		Description: "Neural web search & research via Exa AI (exa-mcp-server)",
		RequiredEnv: []string{"EXA_API_KEY"},
		Build:       npxPreset("exa", "exa-mcp-server", "EXA_API_KEY"),
	},
	"tavily": {
		Name:        "tavily",
		Category:    "Web & Search",
		Description: "AI-optimized web search & extraction via Tavily (tavily-mcp)",
		RequiredEnv: []string{"TAVILY_API_KEY"},
		Build:       npxPreset("tavily", "tavily-mcp", "TAVILY_API_KEY"),
	},

	// Knowledge & Reasoning
	"memory": {
		Name:        "memory",
		Category:    "Knowledge & Memory",
		Description: "Knowledge graph memory server (@modelcontextprotocol/server-memory)",
		Build:       npxPreset("memory", "@modelcontextprotocol/server-memory"),
	},
	"sequential-thinking": {
		Name:        "sequential-thinking",
		Category:    "Knowledge & Memory",
		Description: "Structured step-by-step reasoning (@modelcontextprotocol/server-sequential-thinking)",
		Build:       npxPreset("sequential-thinking", "@modelcontextprotocol/server-sequential-thinking"),
	},
	"context7": {
		Name:        "context7",
		Category:    "Knowledge & Memory",
		Description: "Up-to-date library & framework documentation (@upstash/context7-mcp)",
		Build:       npxPreset("context7", "@upstash/context7-mcp"),
	},
	"time": {
		Name:        "time",
		Category:    "Knowledge & Memory",
		Description: "Timezone conversion and current time lookup (mcp-server-time)",
		Build:       uvxPreset("time", "mcp-server-time"),
	},

	// Commerce, Media & Smart Home
	"stripe": {
		Name:        "stripe",
		Category:    "Commerce & Media",
		Description: "Stripe customers, invoices, subscriptions, and payments (@stripe/mcp)",
		RequiredEnv: []string{"STRIPE_SECRET_KEY"},
		Build:       npxPreset("stripe", "@stripe/mcp", "STRIPE_SECRET_KEY"),
	},
	"figma": {
		Name:        "figma",
		Category:    "Commerce & Media",
		Description: "Figma design files, components, and tokens (figma-developer-mcp)",
		RequiredEnv: []string{"FIGMA_API_KEY"},
		Build:       npxPreset("figma", "figma-developer-mcp", "FIGMA_API_KEY"),
	},
	"home-assistant": {
		Name:        "home-assistant",
		Category:    "Commerce & Media",
		Description: "Home Assistant entities, automations, and device control (homeassistant-mcp)",
		RequiredEnv: []string{"HA_URL", "HA_TOKEN"},
		Build:       uvxPreset("home-assistant", "homeassistant-mcp", "HA_URL", "HA_TOKEN"),
	},
	"spotify": {
		Name:        "spotify",
		Category:    "Commerce & Media",
		Description: "Spotify playback, playlists, and search (spotify-mcp)",
		RequiredEnv: []string{"SPOTIFY_CLIENT_ID", "SPOTIFY_CLIENT_SECRET"},
		Build:       uvxPreset("spotify", "spotify-mcp", "SPOTIFY_CLIENT_ID", "SPOTIFY_CLIENT_SECRET"),
	},
	"blender": {
		Name:        "blender",
		Category:    "Commerce & Media",
		Description: "3D scene inspection & scripting in Blender (blender-mcp)",
		Build:       uvxPreset("blender", "blender-mcp"),
	},

	// Browser & Desktop
	"playwright": {
		Name:        "playwright",
		Category:    "Browser & Web",
		Description: "Headless/headed browser automation (@playwright/mcp)",
		Build: func(args []string) ServerEndpoint {
			display := ""
			if len(args) > 0 {
				display = args[0]
			}
			return PlaywrightPreset(display, false)
		},
	},
	"chrome-devtools": {
		Name:        "chrome-devtools",
		Category:    "Browser & Web",
		Description: "Chrome DevTools Protocol debugging, console & network (chrome-devtools-mcp)",
		Build: func(_ []string) ServerEndpoint {
			return ChromeDevToolsPreset(true)
		},
	},
	"cua-desktop": {
		Name:        "cua-desktop",
		Category:    "Virtual Desktop",
		Description: "Desktop GUI automation via AT-SPI/AX/UIAutomation (cua-driver mcp)",
		Build: func(_ []string) ServerEndpoint {
			return CUADriverPreset(false)
		},
	},
}

// ListPresets returns all curated MCP connector presets sorted by name.
func ListPresets() []PresetInfo {
	names := make([]string, 0, len(presetCatalog))
	for k := range presetCatalog {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]PresetInfo, 0, len(names))
	for _, n := range names {
		out = append(out, presetCatalog[n])
	}
	return out
}

// GetPreset builds a ServerEndpoint for the named curated preset.
func GetPreset(name string, args ...string) (ServerEndpoint, PresetInfo, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	info, ok := presetCatalog[key]
	if !ok {
		return ServerEndpoint{}, PresetInfo{}, fmt.Errorf("unknown MCP preset %q (run `soma mcp presets` to list available presets)", name)
	}
	return info.Build(args), info, nil
}
