package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"github.com/vercel/bridge/pkg/container"
	"github.com/vercel/bridge/pkg/identity"
	"github.com/vercel/bridge/pkg/interact"
	"k8s.io/client-go/tools/clientcmd"
)

// Debug returns the CLI command for collecting diagnostic information.
func Debug() *cli.Command {
	return &cli.Command{
		Name:  "debug",
		Usage: "Collect diagnostic information into a single file",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "output",
				Aliases: []string{"o"},
				Usage:   "Output file path (default: bridge-debug-<timestamp>.txt in current dir)",
			},
			&cli.BoolFlag{
				Name:  "stdout",
				Usage: "Print diagnostic output to stdout instead of writing to a file",
			},
		},
		Action: runDebug,
	}
}

func runDebug(ctx context.Context, c *cli.Command) error {
	p := interact.NewPrinter(c.Root().Writer)

	outputPath := c.String("output")
	if outputPath == "" {
		outputPath = fmt.Sprintf("bridge-debug-%s.txt", time.Now().Format("20060102-150405"))
	}

	var b strings.Builder

	collectHeader(&b)
	collectDeviceIdentity(&b)
	collectKubeContext(&b)
	collectHostLogs(&b)
	ct := container.NewDockerClient()
	containers := collectRunningContainers(ctx, &b, ct)
	collectContainerDiagnostics(ctx, &b, ct, containers)

	if c.Bool("stdout") {
		fmt.Fprint(c.Root().Writer, b.String())
		return nil
	}

	if err := os.WriteFile(outputPath, []byte(b.String()), 0644); err != nil {
		return fmt.Errorf("failed to write debug output: %w", err)
	}

	p.Success(fmt.Sprintf("Debug info written to %s", outputPath))
	return nil
}

func writeSection(b *strings.Builder, name string, content string) {
	fmt.Fprintf(b, "=== %s ===\n", name)
	b.WriteString(content)
	if !strings.HasSuffix(content, "\n") {
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
}

func collectHeader(b *strings.Builder) {
	content := fmt.Sprintf("Timestamp: %s\nVersion:   %s\nOS/Arch:   %s/%s\n",
		time.Now().Format(time.RFC3339),
		Version,
		runtime.GOOS,
		runtime.GOARCH,
	)
	writeSection(b, "HEADER", content)
}

func collectDeviceIdentity(b *strings.Builder) {
	deviceID, err := identity.GetDeviceID()
	if err != nil {
		writeSection(b, "DEVICE IDENTITY", fmt.Sprintf("error: %v", err))
		return
	}
	writeSection(b, "DEVICE IDENTITY", deviceID)
}

func collectKubeContext(b *strings.Builder) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
	rawConfig, err := kubeConfig.RawConfig()
	if err != nil {
		writeSection(b, "KUBERNETES CONTEXT", fmt.Sprintf("error: %v", err))
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Context:   %s\n", rawConfig.CurrentContext)
	if ctxObj, ok := rawConfig.Contexts[rawConfig.CurrentContext]; ok {
		if cluster, ok := rawConfig.Clusters[ctxObj.Cluster]; ok {
			fmt.Fprintf(&sb, "Server:    %s\n", cluster.Server)
		}
		ns := ctxObj.Namespace
		if ns == "" {
			ns = "default"
		}
		fmt.Fprintf(&sb, "Namespace: %s\n", ns)
	}
	writeSection(b, "KUBERNETES CONTEXT", sb.String())
}

func collectHostLogs(b *strings.Builder) {
	home, err := os.UserHomeDir()
	if err != nil {
		writeSection(b, "HOST LOGS", fmt.Sprintf("error: %v", err))
		return
	}

	logPath := filepath.Join(home, ".bridge", "logs", "bridge.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		writeSection(b, "HOST LOGS", fmt.Sprintf("error: %v", err))
		return
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	const tailLines = 200
	if len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
	}
	writeSection(b, "HOST LOGS", fmt.Sprintf("(last %d lines of %s)\n%s", min(len(lines), tailLines), logPath, strings.Join(lines, "\n")))
}

// collectRunningContainers lists bridge containers and returns their IDs.
func collectRunningContainers(ctx context.Context, b *strings.Builder, ct container.Client) []string {
	output, ids, err := ct.List(ctx, container.ListOpts{
		Labels: map[string]string{labelBridgeDeployment: ""},
	})
	if err != nil {
		writeSection(b, "RUNNING BRIDGE CONTAINERS", fmt.Sprintf("error: %v", err))
		return nil
	}
	if output == "" {
		writeSection(b, "RUNNING BRIDGE CONTAINERS", "(none)")
		return nil
	}
	writeSection(b, "RUNNING BRIDGE CONTAINERS", output)
	return ids
}

func collectContainerDiagnostics(ctx context.Context, b *strings.Builder, ct container.Client, containerIDs []string) {
	for _, id := range containerIDs {
		short := id
		if len(short) > 12 {
			short = short[:12]
		}

		sectionPrefix := fmt.Sprintf("CONTAINER %s", short)

		// Intercept log
		writeSection(b, sectionPrefix+" INTERCEPT LOG", ctExec(ctx, ct, id, "cat", "/tmp/bridge-intercept.log"))

		// iptables rules
		writeSection(b, sectionPrefix+" IPTABLES", ctExec(ctx, ct, id, "iptables", "-t", "nat", "-L", "BRIDGE_INTERCEPT", "-n", "-v"))

		// Bridge env
		writeSection(b, sectionPrefix+" BRIDGE ENV", ctExec(ctx, ct, id, "cat", "/etc/profile.d/bridge.sh"))

		// Port listeners
		writeSection(b, sectionPrefix+" PORT LISTENERS", ctExec(ctx, ct, id, "sh", "-c", "ss -tlnp 2>/dev/null || netstat -tlnp 2>/dev/null"))
	}
}

func ctExec(ctx context.Context, ct container.Client, containerID string, args ...string) string {
	out, err := ct.Exec(ctx, containerID, args...)
	if err != nil {
		return fmt.Sprintf("error: %v\n%s", err, out)
	}
	result := strings.TrimSpace(out)
	if result == "" {
		return "(empty)"
	}
	return result
}
