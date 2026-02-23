package commands

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/vercel-eddie/bridge/api/go/bridge/v1"
	"github.com/vercel-eddie/bridge/pkg/devcontainer"
	"github.com/vercel-eddie/bridge/pkg/identity"
	"github.com/vercel-eddie/bridge/pkg/k8s/k8spf"
	"k8s.io/client-go/tools/clientcmd"
)

const defaultFeatureRef = "ghcr.io/vercel-eddie/bridge/features/bridge:edge"
const devFeatureRef = "../local-features/bridge"

const defaultAdminAddr = "k8spf:///administrator.bridge:9090?workload=deployment"

// Create returns the CLI command for creating a bridge.
func Create() *cli.Command {
	return &cli.Command{
		Name:  "create",
		Usage: "Create a bridge to a Kubernetes deployment",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "connect",
				Usage: "Start a Devcontainer and connect to the bridge after creation",
			},
			&cli.StringFlag{
				Name:    "namespace",
				Aliases: []string{"n"},
				Usage:   "Source namespace of the deployment",
				Sources: cli.EnvVars("BRIDGE_SOURCE_NAMESPACE"),
			},
			&cli.StringFlag{
				Name:    "admin-addr",
				Usage:   "Address of the bridge administrator (e.g. localhost:9090 or k8spf:///pod.ns:9090)",
				Value:   defaultAdminAddr,
				Sources: cli.EnvVars("BRIDGE_ADMIN_ADDR"),
			},
			&cli.BoolFlag{
				Name:  "force",
				Usage: "Force recreation without confirmation",
			},
			&cli.StringFlag{
				Name:    "devcontainer-config",
				Aliases: []string{"f"},
				Usage:   "Path to the base devcontainer.json config file",
			},
			&cli.IntFlag{
				Name:    "listen",
				Aliases: []string{"l"},
				Usage:   "App listening port to forward inbound requests to",
				Value:   3000,
			},
			&cli.StringFlag{
				Name:    "feature-ref",
				Usage:   "Devcontainer feature reference for the bridge feature",
				Value:   defaultFeatureRef,
				Hidden:  true,
				Sources: cli.EnvVars("BRIDGE_FEATURE_REF"),
			},
		},
		Arguments: []cli.Argument{
			&cli.StringArg{
				Name:      "deployment",
				UsageText: "Name of the source Deployment to bridge (optional)",
				Config: cli.StringConfig{
					TrimSpace: true,
				},
			},
		},
		Action: runCreate,
	}
}

func runCreate(ctx context.Context, c *cli.Command) error {
	deploymentName := c.StringArg("deployment")
	sourceNamespace := c.String("namespace")
	if sourceNamespace == "" && deploymentName != "" {
		sourceNamespace = currentKubeNamespace()
	}
	adminAddr := c.String("admin-addr")
	connectFlag := c.Bool("connect")
	force := c.Bool("force")
	featureRef := c.String("feature-ref")
	if featureRef == defaultFeatureRef && Version == "dev" {
		featureRef = devFeatureRef
	}

	w := c.Root().Writer
	r := c.Root().Reader

	// Step 1: Resolve device identity
	deviceID, err := identity.GetDeviceID()
	if err != nil {
		return fmt.Errorf("failed to get device identity: %w", err)
	}
	slog.Info("Device identity", "device_id", deviceID)

	// Step 2: Connect to the administrator
	slog.Info("Connecting to bridge administrator...", "addr", adminAddr)

	builder := k8spf.NewBuilder(k8spf.BuilderConfig{})
	conn, err := grpc.NewClient(adminAddr,
		append(builder.DialOptions(), grpc.WithTransportCredentials(insecure.NewCredentials()))...,
	)
	if err != nil {
		return fmt.Errorf("failed to connect to administrator: %w", err)
	}
	defer conn.Close()

	client := pb.NewAdministratorServiceClient(conn)

	// Step 4: Check for existing bridges
	listResp, err := client.ListBridges(ctx, &pb.ListBridgesRequest{
		DeviceId: deviceID,
	})
	if err != nil {
		slog.Warn("Failed to list existing bridges", "error", err)
	} else if len(listResp.Bridges) > 0 && !force {
		// Check if there's an existing bridge for the same deployment
		for _, bridge := range listResp.Bridges {
			if bridge.SourceDeployment == deploymentName {
				fmt.Fprintf(w, "\nWarning: An existing bridge for deployment %q already exists in namespace %q (created %s).\n",
					bridge.SourceDeployment, bridge.Namespace, bridge.CreatedAt)
				fmt.Fprintf(w, "This will tear down the existing bridge and recreate it.\n")
				fmt.Fprintf(w, "Continue? [y/N] ")

				reader := bufio.NewReader(r)
				answer, _ := reader.ReadString('\n')
				answer = strings.TrimSpace(strings.ToLower(answer))
				if answer != "y" && answer != "yes" {
					fmt.Fprintf(w, "Aborted.\n")
					return nil
				}
				force = true
				break
			}
		}
	}

	// Step 5: Create the bridge
	slog.Info("Creating bridge...",
		"deployment", deploymentName,
		"source_namespace", sourceNamespace,
	)

	createResp, err := client.CreateBridge(ctx, &pb.CreateBridgeRequest{
		DeviceId:         deviceID,
		SourceDeployment: deploymentName,
		SourceNamespace:  sourceNamespace,
		Force:            force,
	})
	if err != nil {
		return fmt.Errorf("failed to create bridge: %w", err)
	}

	fmt.Fprintf(w, "\nBridge created successfully!\n")
	fmt.Fprintf(w, "  Namespace: %s\n", createResp.Namespace)
	fmt.Fprintf(w, "  Pod:       %s\n", createResp.PodName)
	fmt.Fprintf(w, "  Port:      %d\n", createResp.Port)

	// Step 6: Generate devcontainer config when a base config is provided.
	baseConfigFlag := c.String("devcontainer-config")
	if baseConfigFlag != "" || connectFlag {
		baseConfig, err := devcontainer.ResolveConfigPath(baseConfigFlag)
		if err != nil {
			return err
		}
		dcConfigPath, err := generateDevcontainerConfig(w, deploymentName, baseConfig, featureRef, c.Int("listen"), createResp)
		if err != nil {
			return err
		}
		if connectFlag {
			return startDevcontainer(ctx, dcConfigPath, r, w)
		}
	}

	return nil
}

// generateDevcontainerConfig creates a bridge devcontainer.json from a base config.
// It respects the KUBECONFIG env var by bind-mounting it into the container,
// unless the base config already sets containerEnv.KUBECONFIG.
// Returns the path to the generated config.
func generateDevcontainerConfig(w io.Writer, deploymentName, baseConfigPath, featureRef string, appPort int, resp *pb.CreateBridgeResponse) (string, error) {
	dcName := deploymentName
	if dcName == "" {
		dcName = "proxy"
	}

	// Place the generated config under the .devcontainer/ directory that contains
	// the base config. If the base config isn't already in a .devcontainer/ folder,
	// create one next to it.
	baseParent := filepath.Dir(baseConfigPath)
	var dcDir string
	if filepath.Base(baseParent) == ".devcontainer" {
		// Base is at <workspace>/.devcontainer/devcontainer.json — use the same .devcontainer/.
		dcDir = filepath.Join(baseParent, fmt.Sprintf("bridge-%s", dcName))
	} else {
		// Base is elsewhere — create a .devcontainer/ directory next to it.
		dcDir = filepath.Join(baseParent, ".devcontainer", fmt.Sprintf("bridge-%s", dcName))
	}
	dcConfigPath := filepath.Join(dcDir, "devcontainer.json")

	// Load from base config, then overlay bridge settings.
	cfg, err := devcontainer.Load(baseConfigPath)
	if err != nil {
		return "", fmt.Errorf("failed to load base devcontainer config: %w", err)
	}

	cfg.Name = "bridge-" + dcName
	bridgeServerAddr := fmt.Sprintf("k8spf:///%s.%s:%d?workload=deployment", resp.DeploymentName, resp.Namespace, resp.Port)
	cfg.SetFeature(featureRef, map[string]any{
		"bridgeVersion":    Version,
		"bridgeServerAddr": bridgeServerAddr,
		"forwardDomains":   "*",
		"appPort":          fmt.Sprintf("%d", appPort),
		"workspacePath":    "${containerWorkspaceFolder}",
	})
	cfg.EnsureCapAdd("NET_ADMIN")

	if err := configureDevMounts(cfg); err != nil {
		return "", err
	}

	if err := os.MkdirAll(dcDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create devcontainer directory: %w", err)
	}
	if err := cfg.Save(dcConfigPath); err != nil {
		return "", fmt.Errorf("failed to write devcontainer config: %w", err)
	}

	// Ensure generated bridge config directories are gitignored.
	ensureGitignore(baseParent, "bridge-*/")

	fmt.Fprintf(w, "\nDevcontainer config written to %s\n", dcConfigPath)
	return dcConfigPath, nil
}

// currentKubeNamespace returns the current Kubernetes namespace.
// In-cluster it reads /var/run/secrets/kubernetes.io/serviceaccount/namespace;
// out-of-cluster it reads the kubeconfig context. Falls back to "default".
func currentKubeNamespace() string {
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
	ns, _, err := kubeConfig.Namespace()
	if err != nil || ns == "" {
		return "default"
	}
	return ns
}

// configureDevMounts adds host bind mounts needed for local development:
// the linux bridge binary (dev mode), KUBECONFIG, and Docker network access.
func configureDevMounts(cfg *devcontainer.Config) error {
	// In dev mode, bind-mount the linux bridge binary into the container,
	// unless the base config already provides one (e.g. in e2e tests).
	if Version == "dev" && !hasMountTarget(cfg, "/usr/local/bin/bridge") {
		binPath, err := filepath.Abs(filepath.Join("dist", "bridge-linux"))
		if err != nil {
			return fmt.Errorf("failed to resolve bridge binary path: %w", err)
		}
		if _, err := os.Stat(binPath); err != nil {
			return fmt.Errorf("dev mode requires a linux bridge binary at %s — build with: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags=\"-s -w\" -o dist/bridge-linux ./cmd/bridge", binPath)
		}
		cfg.SetMount(fmt.Sprintf("source=%s,target=/usr/local/bin/bridge,type=bind,readonly", binPath))
	}

	// Mount KUBECONFIG unless the base config already configured it.
	if _, exists := cfg.ContainerEnv["KUBECONFIG"]; !exists {
		if err := configureKubeconfig(cfg); err != nil {
			return err
		}
	}

	return nil
}

// configureKubeconfig mounts a kubeconfig into the devcontainer. If any cluster
// server URL points to localhost/0.0.0.0/127.0.0.1, it rewrites it to
// host.docker.internal so it's reachable from inside the container.
func configureKubeconfig(cfg *devcontainer.Config) error {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})

	rawConfig, err := kubeConfig.RawConfig()
	if err != nil {
		return nil // no kubeconfig available, skip
	}

	mountTarget := "/tmp/bridge-kubeconfig"

	// Rewrite localhost URLs so they're reachable from inside Docker.
	needsRewrite := false
	for _, cluster := range rawConfig.Clusters {
		for _, local := range []string{"://0.0.0.0:", "://127.0.0.1:", "://localhost:"} {
			if strings.Contains(cluster.Server, local) {
				cluster.Server = strings.Replace(cluster.Server, local, "://host.docker.internal:", 1)
				cluster.InsecureSkipTLSVerify = true
				cluster.CertificateAuthorityData = nil
				needsRewrite = true
				break
			}
		}
	}

	if needsRewrite {
		rewritten, err := clientcmd.Write(rawConfig)
		if err != nil {
			return fmt.Errorf("failed to rewrite kubeconfig: %w", err)
		}
		tmpFile, err := os.CreateTemp("", "bridge-kubeconfig-*")
		if err != nil {
			return fmt.Errorf("failed to create temp kubeconfig: %w", err)
		}
		if _, err := tmpFile.Write(rewritten); err != nil {
			tmpFile.Close()
			return fmt.Errorf("failed to write temp kubeconfig: %w", err)
		}
		tmpFile.Close()
		cfg.SetMount(fmt.Sprintf("source=%s,target=%s,type=bind,readonly", tmpFile.Name(), mountTarget))
	} else {
		kubeconfigPath := os.Getenv("KUBECONFIG")
		if kubeconfigPath == "" {
			home, _ := os.UserHomeDir()
			if home != "" {
				defaultPath := filepath.Join(home, ".kube", "config")
				if _, err := os.Stat(defaultPath); err == nil {
					kubeconfigPath = defaultPath
				}
			}
		}
		if kubeconfigPath == "" {
			return nil
		}
		absPath, err := filepath.Abs(kubeconfigPath)
		if err != nil {
			return fmt.Errorf("failed to resolve KUBECONFIG path: %w", err)
		}
		cfg.SetMount(fmt.Sprintf("source=%s,target=%s,type=bind,readonly", absPath, mountTarget))
	}

	cfg.EnsureContainerEnv("KUBECONFIG", mountTarget)
	return nil
}

// hasMountTarget returns true if any existing mount in cfg targets the given path.
func hasMountTarget(cfg *devcontainer.Config, target string) bool {
	needle := "target=" + target
	for _, m := range cfg.Mounts {
		if strings.Contains(m, needle) {
			return true
		}
	}
	return false
}

// ensureGitignore adds a pattern to the .gitignore file in dir if not already present.
func ensureGitignore(dir, pattern string) {
	gitignorePath := filepath.Join(dir, ".gitignore")

	data, err := os.ReadFile(gitignorePath)
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == pattern {
				return
			}
		}
	}

	f, err := os.OpenFile(gitignorePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		slog.Warn("Failed to update .gitignore", "path", gitignorePath, "error", err)
		return
	}
	defer f.Close()

	// Add a newline before the pattern if the file doesn't end with one.
	if len(data) > 0 && data[len(data)-1] != '\n' {
		f.WriteString("\n")
	}
	f.WriteString(pattern + "\n")
}

// startDevcontainer starts the devcontainer and attaches an interactive shell.
func startDevcontainer(ctx context.Context, dcConfigPath string, r io.Reader, w io.Writer) error {
	// <workspace>/.devcontainer/bridge-<name>/devcontainer.json → <workspace>
	workspaceFolder := filepath.Dir(filepath.Dir(filepath.Dir(dcConfigPath)))
	dcClient := &devcontainer.Client{
		WorkspaceFolder: workspaceFolder,
		ConfigPath:      dcConfigPath,
		Stdin:           r,
		Stdout:          w,
		Stderr:          w,
	}

	slog.Info("Starting devcontainer", "config", dcConfigPath, "workspace", workspaceFolder)
	if err := dcClient.Up(ctx); err != nil {
		return fmt.Errorf("failed to start devcontainer: %w", err)
	}

	slog.Info("Devcontainer started, attaching shell")
	return dcClient.ExecAttached(ctx, []string{"bash"})
}
