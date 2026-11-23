package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ra1ncandy/soma/internal/desktop"
)

func newDesktopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "desktop",
		Short: "Manage soma's isolated Docker virtual desktop (Xvfb + XFCE + noVNC + AT-SPI2 MCP)",
	}
	cmd.AddCommand(
		newDesktopStartCmd(),
		newDesktopStopCmd(),
		newDesktopStatusCmd(),
		newDesktopOpenCmd(),
	)
	return cmd
}

func newDesktopStartCmd() *cobra.Command {
	var wsDir, image, dockerDir, resolution, netMode string
	var port int
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start an isolated Docker desktop container for the workspace",
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr := desktop.NewManager()
			st, err := mgr.Start(cmd.Context(), desktop.Config{
				WorkspaceDir: wsDir,
				Image:        image,
				DockerDir:    dockerDir,
				NoVNCPort:    port,
				Resolution:   resolution,
				NetworkMode:  netMode,
			})
			if err != nil {
				return err
			}
			fmt.Printf("Started isolated desktop container %s (%s)\n", st.ContainerName, st.Resolution)
			fmt.Printf("noVNC viewer (loopback-only): %s\n", st.NoVNCURL())
			return nil
		},
	}
	cmd.Flags().StringVar(&wsDir, "dir", ".", "Workspace directory mounted at /workspace")
	cmd.Flags().StringVar(&image, "image", desktop.DefaultImage, "Docker desktop image name")
	cmd.Flags().StringVar(&dockerDir, "docker-dir", "", "Optional path to docker/desktop/ for automatic image build")
	cmd.Flags().StringVar(&resolution, "resolution", desktop.DefaultResolution, "Virtual screen resolution (WxH)")
	cmd.Flags().StringVar(&netMode, "network", "bridge", "Docker network mode (bridge | none)")
	cmd.Flags().IntVar(&port, "port", desktop.DefaultPort, "Loopback port for noVNC viewer")
	return cmd
}

func newDesktopStopCmd() *cobra.Command {
	var wsDir string
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop and remove the workspace's isolated desktop container",
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr := desktop.NewManager()
			if err := mgr.Stop(cmd.Context(), wsDir); err != nil {
				return err
			}
			fmt.Println("Stopped isolated desktop container.")
			return nil
		},
	}
	cmd.Flags().StringVar(&wsDir, "dir", ".", "Workspace directory")
	return cmd
}

func newDesktopStatusCmd() *cobra.Command {
	var wsDir string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show status and noVNC URL of the isolated desktop container",
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr := desktop.NewManager()
			st, running, err := mgr.Status(cmd.Context(), wsDir)
			if err != nil {
				return err
			}
			if !running {
				fmt.Printf("Desktop container %s is not running.\n", st.ContainerName)
				return nil
			}
			fmt.Printf("Desktop container %s is RUNNING (%s)\n", st.ContainerName, st.Resolution)
			fmt.Printf("noVNC URL: %s\n", st.NoVNCURL())
			return nil
		},
	}
	cmd.Flags().StringVar(&wsDir, "dir", ".", "Workspace directory")
	return cmd
}

func newDesktopOpenCmd() *cobra.Command {
	var wsDir string
	cmd := &cobra.Command{
		Use:   "open",
		Short: "Print the authenticated local noVNC URL for the running desktop",
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr := desktop.NewManager()
			st, running, err := mgr.Status(cmd.Context(), wsDir)
			if err != nil {
				return err
			}
			if !running {
				return fmt.Errorf("desktop container %s is not running (run 'soma desktop start' first)", st.ContainerName)
			}
			fmt.Println(st.NoVNCURL())
			return nil
		},
	}
	cmd.Flags().StringVar(&wsDir, "dir", ".", "Workspace directory")
	return cmd
}
