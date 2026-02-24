package commands

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/vercel/bridge/pkg/admin"
	"github.com/vercel/bridge/pkg/devcontainer"
	"github.com/vercel/bridge/pkg/identity"
	"github.com/vercel/bridge/pkg/interact"
)

const defaultFeatureRef = "ghcr.io/vercel/bridge/bridge-feature:latest"
const devFeatureRef = "../local-features/bridge-feature"

const defaultAdminAddr = "k8spf:///administrator.bridge:9090?workload=deployment"
const defaultProxyImage = "ghcr.io/vercel/bridge-cli:edge"

// Create returns the CLI command for creating a bridge.
func Create() *cli.Command {
	return &cli.Command{
		Name:  "create",
		Usage: "Create a bridge to a Kubernetes deployment",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "connect",
				Aliases: []string{"c"},
				Usage:   "Start a Devcontainer and connect to the bridge after creation",
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
				Name:    "yes",
				Aliases: []string{"y"},
				Usage:   "Auto-accept all confirmation prompts",
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
			&cli.StringFlag{
				Name:    "proxy-image",
				Usage:   "Bridge proxy container image (used for local admin fallback)",
				Value:   defaultProxyImage,
				Hidden:  true,
				Sources: cli.EnvVars("BRIDGE_PROXY_IMAGE"),
			},
		},
		Before: preflightCreate,
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

// errAdminUnavailable is a sentinel error returned when the remote
// administrator cannot be reached.
type errAdminUnavailable struct{}

func (errAdminUnavailable) Error() string { return "administrator unavailable" }

// preflightCreate runs pre-flight checks before the create command executes.
func preflightCreate(ctx context.Context, c *cli.Command) (context.Context, error) {
	if err := checkKubeconfig(); err != nil {
		return ctx, err
	}
	if c.Bool("connect") {
		if err := checkDocker(ctx); err != nil {
			return ctx, err
		}
	}
	return ctx, nil
}

// checkKubeconfig verifies that a kubeconfig file exists.
func checkKubeconfig() error {
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" {
		home, _ := os.UserHomeDir()
		if home != "" {
			kubeconfigPath = filepath.Join(home, ".kube", "config")
		}
	}
	if kubeconfigPath == "" {
		return fmt.Errorf("no kubeconfig found: set the KUBECONFIG environment variable or create ~/.kube/config")
	}
	if _, err := os.Stat(kubeconfigPath); err != nil {
		return fmt.Errorf("kubeconfig not found at %s: set the KUBECONFIG environment variable or run 'kubectl config view' to verify your setup", kubeconfigPath)
	}
	return nil
}

// checkDocker verifies that the Docker daemon is running.
func checkDocker(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "info")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Docker is not running: the --connect flag requires Docker to start a devcontainer. Please start Docker Desktop or the Docker daemon and try again")
	}
	return nil
}

func runCreate(ctx context.Context, c *cli.Command) error {
	deploymentName := c.StringArg("deployment")
	sourceNamespace := c.String("namespace")
	if sourceNamespace == "" && deploymentName != "" {
		sourceNamespace = currentKubeNamespace()
	}
	adminAddr := c.String("admin-addr")
	connectFlag := c.Bool("connect")
	yes := c.Bool("yes")
	proxyImage := c.String("proxy-image")
	featureRef := c.String("feature-ref")
	if featureRef == defaultFeatureRef && Version == "dev" {
		featureRef = devFeatureRef
	}

	r := c.Root().Reader
	p := interact.NewPrinter(c.Root().Writer)

	// Step 1: Resolve device identity.
	deviceID, err := identity.GetDeviceID()
	if err != nil {
		return fmt.Errorf("failed to get device identity: %w", err)
	}
	slog.Info("Device identity", "device_id", deviceID)

	kubeContext := currentKubeContext()

	// Step 2: Connect to administrator (remote, with local fallback).
	var adm admin.Admin
	var existingBridges []*admin.BridgeInfo

	sp := interact.NewSpinner("Connecting to bridge administrator...")
	go sp.Start(ctx)

	remote, dialErr := admin.NewRemote(adminAddr)
	if dialErr == nil {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		bridges, probeErr := remote.ListBridges(probeCtx, deviceID)
		cancel()
		if probeErr == nil {
			existingBridges = bridges
			adm = remote
		} else {
			remote.Close()
		}
	}
	sp.Stop()

	if adm == nil {
		// Remote admin not available — offer local fallback.
		if !yes {
			p.Newline()
			p.Warn("No bridge administrator found in the cluster.")
			p.Info(fmt.Sprintf("Should Bridge use your local credentials for cluster %q instead.", kubeContext))
			p.Prompt("Continue? [y/N] ")

			if answer := promptYN(r); answer != "y" && answer != "yes" {
				p.Println("Aborted.")
				return nil
			}
		}

		sp = interact.NewSpinner("Initializing local administrator...")
		go sp.Start(ctx)

		localAdm, localErr := admin.NewLocal(admin.LocalConfig{
			ProxyImage: proxyImage,
		})
		if localErr != nil {
			sp.Stop()
			return fmt.Errorf("failed to initialize: %w", localErr)
		}
		adm = localAdm
		bridges, listErr := localAdm.ListBridges(ctx, deviceID)
		if listErr != nil {
			slog.Warn("Failed to list existing bridges", "error", listErr)
		} else {
			existingBridges = bridges
		}

		sp.Stop()
	}
	defer adm.Close()

	// Step 3: Check for existing bridges.
	if !yes {
		for _, bridge := range existingBridges {
			if bridge.SourceDeployment == deploymentName {
				p.Newline()
				p.Warn("An existing bridge already exists:")
				p.KeyValue("Name", bridge.SourceDeployment)
				p.KeyValue("Created", bridge.CreatedAt)
				p.KeyValue("Context", kubeContext)
				p.Newline()
				p.Muted("This will tear down the existing bridge and recreate it.")
				p.Prompt("Continue? [y/N] ")

				if answer := promptYN(r); answer != "y" && answer != "yes" {
					p.Println("Aborted.")
					return nil
				}
				yes = true
				break
			}
		}
	}

	// Step 4: Create bridge.
	var createResp *admin.CreateResponse

	sp = interact.NewSpinner("Creating bridge...")
	go sp.Start(ctx)

	createResp, err = adm.CreateBridge(ctx, admin.CreateRequest{
		DeviceID:         deviceID,
		SourceDeployment: deploymentName,
		SourceNamespace:  sourceNamespace,
		Force:            yes,
	})
	sp.Stop()
	if err != nil {
		return err
	}

	p.Newline()
	p.Success("Bridge created successfully!")
	p.KeyValue("Namespace", createResp.Namespace)
	p.KeyValue("Pod", createResp.PodName)
	p.KeyValue("Port", fmt.Sprintf("%d", createResp.Port))
	p.KeyValue("Context", kubeContext)
	p.Newline()

	// Step 5: Generate devcontainer config when a base config is provided.
	baseConfigFlag := c.String("devcontainer-config")
	if baseConfigFlag != "" || connectFlag {
		baseConfig, err := devcontainer.ResolveConfigPath(baseConfigFlag)
		if err != nil {
			return err
		}
		dcConfigPath, err := generateDevcontainerConfig(p, baseConfig, featureRef, c.Int("listen"), createResp)
		if err != nil {
			return err
		}
		if connectFlag {
			return startDevcontainer(ctx, p, dcConfigPath, r)
		}
	}

	return nil
}

// promptYN reads a single line from r and returns the trimmed, lowercased answer.
func promptYN(r io.Reader) string {
	reader := bufio.NewReader(r)
	answer, _ := reader.ReadString('\n')
	return strings.TrimSpace(strings.ToLower(answer))
}

// generateDevcontainerConfig creates a bridge devcontainer.json from a base config.
// It respects the KUBECONFIG env var by bind-mounting it into the container,
// unless the base config already sets containerEnv.KUBECONFIG.
// Returns the path to the generated config.
func generateDevcontainerConfig(p interact.Printer, baseConfigPath, featureRef string, appPort int, resp *admin.CreateResponse) (string, error) {
	dcName := resp.DeploymentName

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
	// Normalize "edge-<commit>" to "edge" so the feature install script
	// downloads from the correct GitHub release tag.
	featureVersion := Version
	if strings.HasPrefix(featureVersion, "edge-") {
		featureVersion = "edge"
	}
	cfg.SetFeature(featureRef, map[string]any{
		"bridgeVersion":    featureVersion,
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

	// Write source deployment env vars to a .env file so the devcontainer
	// gets them injected via --env-file.
	if len(resp.EnvVars) > 0 {
		envFilePath := filepath.Join(dcDir, "development.env")
		if err := writeEnvFile(envFilePath, resp.EnvVars); err != nil {
			return "", fmt.Errorf("failed to write env file: %w", err)
		}
		cfg.EnsureRunArgs("--env-file", envFilePath)
		p.Info(fmt.Sprintf("Environment variables written to %s (%d vars)", envFilePath, len(resp.EnvVars)))
	}

	if err := cfg.Save(dcConfigPath); err != nil {
		return "", fmt.Errorf("failed to write devcontainer config: %w", err)
	}

	// Ensure generated bridge config directories are gitignored.
	ensureGitignore(baseParent, "bridge-*/")

	p.Info(fmt.Sprintf("Devcontainer config written to %s", dcConfigPath))
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
		return fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	// Resolve the kubeconfig file path for mounting.
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" {
		home, _ := os.UserHomeDir()
		if home != "" {
			kubeconfigPath = filepath.Join(home, ".kube", "config")
		}
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

// writeEnvFile writes a map of environment variables to a .env file.
// Keys are sorted for deterministic output.
func writeEnvFile(path string, vars map[string]string) error {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		v := vars[k]
		// Quote values that contain spaces, quotes, or newlines.
		if strings.ContainsAny(v, " \t\n\r\"'\\#") {
			v = "\"" + strings.ReplaceAll(strings.ReplaceAll(v, "\\", "\\\\"), "\"", "\\\"") + "\""
		}
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return os.WriteFile(path, []byte(b.String()), 0600)
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

// currentKubeContext returns the name of the active kubectl context.
func currentKubeContext() string {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
	rawConfig, err := kubeConfig.RawConfig()
	if err != nil {
		return ""
	}
	return rawConfig.CurrentContext
}

// startDevcontainer starts the devcontainer and attaches an interactive shell.
func startDevcontainer(ctx context.Context, p interact.Printer, dcConfigPath string, r io.Reader) error {
	// <workspace>/.devcontainer/bridge-<name>/devcontainer.json → <workspace>
	workspaceFolder := filepath.Dir(filepath.Dir(filepath.Dir(dcConfigPath)))
	dcClient := &devcontainer.Client{
		WorkspaceFolder: workspaceFolder,
		ConfigPath:      dcConfigPath,
		Stdin:           r,
		Stdout:          os.Stdout,
		Stderr:          os.Stderr,
	}

	slog.Debug("Starting devcontainer", "config", dcConfigPath, "workspace", workspaceFolder)

	sp := interact.NewSpinner("Starting devcontainer...")
	go sp.Start(ctx)

	err := dcClient.Up(ctx)
	sp.Stop()
	if err != nil {
		return fmt.Errorf("failed to start devcontainer: %w", err)
	}

	slog.Debug("Devcontainer started, attaching shell")
	return dcClient.ExecAttached(ctx, []string{"bash"})
}
