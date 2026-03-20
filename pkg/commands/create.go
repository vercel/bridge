package commands

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"k8s.io/client-go/tools/clientcmd"

	"buf.build/go/protovalidate"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/archive"
	"github.com/vercel/bridge/pkg/container"
	"github.com/vercel/bridge/pkg/devcontainer"
	"github.com/vercel/bridge/pkg/identity"
	"github.com/vercel/bridge/pkg/interact"
	"github.com/vercel/bridge/pkg/intercept"
	"github.com/vercel/bridge/pkg/k8s/meta"
	"github.com/vercel/bridge/pkg/netutil"
	"github.com/vercel/bridge/pkg/profile"
	"github.com/vercel/bridge/pkg/session"
	"google.golang.org/protobuf/encoding/protojson"
)

const featureRefBase = "ghcr.io/vercel/bridge/bridge-feature"
const devFeatureRef = "../local-features/bridge-feature"

const defaultAdminAddr = "k8spf:///administrator.bridge:9090?workload=deployment"
const defaultProxyImage = "ghcr.io/vercel/bridge-cli:latest"
const labelBridgeDeployment = "bridge.deployment"

const createUsageText = `bridge create <deployment> [flags]

Examples:
  # Create from an existing deployment
  bridge create my-api -n production

  # Create from a directory of Kubernetes manifests
  bridge create --source ./k8s/

  # Create then run a command with bridge exec
  bridge create my-api
  bridge exec my-api -- npm test

  # Create then use devcontainer up and devcontainer exec
  bridge create my-api
  devcontainer up --config .devcontainer/bridge-<name>/devcontainer.json
  devcontainer exec --config .devcontainer/bridge-<name>/devcontainer.json npm test`

const createAgentUsageText = createUsageText + `

Profiles:
  Bridge automatically loads .bridge/profile.json from the current directory
  or any parent directory. Profiles let you define rules that automatically
  set flags (e.g. --namespace, --server-facade, --name) based on CEL
  expressions matched against the command arguments.

  Run "bridge schema profile" to print the JSON schema, or use $schema in
  your profile.json for editor autocompletion:

    {
      "$schema": "https://raw.githubusercontent.com/vercel/bridge/main/api/jsonschema/Profile.schema.json",
      "create": [...]
    }

  Server facade specs also support $schema:

    {
      "$schema": "https://raw.githubusercontent.com/vercel/bridge/main/api/jsonschema/ServerFacade.schema.json",
      "host": "example.com",
      "routes": [...]
    }

Output:
  With --output=json, emits a CommandResult envelope (see "bridge --help").
  Run "bridge schema create-response" for the response payload schema.`

// Create returns the CLI command for creating a bridge.
func Create() *cli.Command {
	usageText := createUsageText
	if interact.IsJSON() {
		usageText = createAgentUsageText
	}
	return &cli.Command{
		Name:      "create",
		Usage:     "Generate a devcontainer connected to a target deployment",
		UsageText: usageText,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "name",
				Usage: "Bridge name (defaults to the workload name)",
			},
			&cli.BoolFlag{
				Name:    "connect",
				Aliases: []string{"c"},
				Usage:   "Start the devcontainer and exec into it after creation",
				Hidden:  interact.IsJSON(),
			},
			&cli.StringFlag{
				Name:    "namespace",
				Aliases: []string{"n"},
				Usage:   "Namespace of the target deployment",
				Sources: cli.EnvVars("BRIDGE_SOURCE_NAMESPACE"),
			},
			&cli.StringFlag{
				Name:    "admin-addr",
				Usage:   "Address of the bridge administrator",
				Value:   defaultAdminAddr,
				Sources: cli.EnvVars("BRIDGE_ADMIN_ADDR"),
			},
			&cli.BoolFlag{
				Name:    "yes",
				Aliases: []string{"y"},
				Usage:   "Auto-accept all confirmation prompts",
				Hidden:  interact.IsJSON(),
			},
			&cli.StringFlag{
				Name:    "devcontainer-config",
				Aliases: []string{"f"},
				Usage:   "Path to a base devcontainer.json to extend",
			},
			&cli.IntFlag{
				Name:    "listen",
				Aliases: []string{"l"},
				Usage:   "Port your app listens on (defaults to PORT env var, then 3000)",
			},
			&cli.StringFlag{
				Name:   "feature-ref",
				Usage:  "Devcontainer feature reference for the bridge feature",
				Hidden: true,
				Sources: cli.NewValueSourceChain(
					cli.EnvVar("BRIDGE_FEATURE_REF"),
					FuncSource(defaultFeatureRef),
				),
			},
			&cli.StringFlag{
				Name:    "proxy-image",
				Usage:   "Bridge proxy container image",
				Value:   defaultProxyImage,
				Hidden:  true,
				Sources: cli.EnvVars("BRIDGE_PROXY_IMAGE"),
			},
			&cli.StringFlag{
				Name:    "source",
				Aliases: []string{"s"},
				Usage:   "Path to Kubernetes manifests (folder, glob, or YAML file)",
			},
			&cli.StringFlag{
				Name:   "container-binary-path",
				Usage:  "Path to the linux bridge binary to mount into the devcontainer",
				Hidden: true,
				Sources: cli.NewValueSourceChain(
					cli.EnvVar("BRIDGE_CONTAINER_BINARY_PATH"),
					FuncSource(linuxBinaryPath),
				),
			},
			&cli.StringSliceFlag{
				Name:  "server-facade",
				Usage: "Server facade spec (JSON string or file path). May be repeated. See `bridge schema server-facade` for the schema.",
			},
			&cli.StringFlag{
				Name:    "devcontainer-up-args",
				Usage:   "Additional arguments to pass to devcontainer up (e.g. \"--rebuild\")",
				Hidden:  interact.IsJSON(),
				Sources: cli.EnvVars("BRIDGE_DEVCONTAINER_UP_ARGS"),
			},
		},
		Before: chainBefore(applyProfile, preflightCreate),
		Arguments: []cli.Argument{
			&cli.StringArg{
				Name:      "workload_name",
				UsageText: "Name of the source workload (defaults to a Deployment)",
				Config: cli.StringConfig{
					TrimSpace: true,
				},
			},
		},
		Action: runCreate,
	}
}

// linuxBinaryPath returns the default path to the linux bridge binary that
// will be bind-mounted into devcontainers. In dev mode it uses the local
// build output; otherwise the installer-managed copy at ~/.bridge/bin/.
func linuxBinaryPath() string {
	if Version == "dev" {
		return filepath.Join("dist", "bridge-linux")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.Getenv("HOME"), ".bridge", "bin", "bridge-linux")
	}
	return filepath.Join(home, ".bridge", "bin", "bridge-linux")
}

// chainBefore composes multiple Before hooks into a single hook.
func chainBefore(fns ...cli.BeforeFunc) cli.BeforeFunc {
	return func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
		for _, fn := range fns {
			var err error
			ctx, err = fn(ctx, cmd)
			if err != nil {
				return ctx, err
			}
		}
		return ctx, nil
	}
}

// applyProfile loads .bridge/profile.json and applies matching create rules.
func applyProfile(ctx context.Context, cmd *cli.Command) (context.Context, error) {
	profilePath := profile.ProfilePath()
	if profilePath == "" {
		return ctx, nil
	}

	p, err := profile.LoadFromFlag(profilePath)
	if err != nil {
		slog.Warn("Failed to load profile", "error", err)
		return ctx, nil
	}
	if p == nil {
		return ctx, nil
	}

	w := cmd.Root().Writer
	printer := interact.NewPrinter(w)
	printer.Infof("Loaded profile from %s", profilePath)

	if err := profile.ApplyCreate(p, cmd); err != nil {
		return ctx, fmt.Errorf("apply profile: %w", err)
	}
	return ctx, nil
}

// preflightCreate runs pre-flight checks before the create command executes.
func preflightCreate(ctx context.Context, c *cli.Command) (context.Context, error) {
	if c.Bool("connect") {
		if err := checkDocker(ctx); err != nil {
			return ctx, err
		}
	}
	return ctx, nil
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
	deploymentName := c.StringArg("workload_name")
	name := c.String("name")
	sourceNamespace := c.String("namespace")
	if sourceNamespace == "" {
		sourceNamespace = currentKubeNamespace()
	}
	adminAddr := c.String("admin-addr")
	connectFlag := c.Bool("connect")
	yes := c.Bool("yes") || interact.IsJSON()
	proxyImage := c.String("proxy-image")
	featureRef := c.String("feature-ref")
	containerBinaryPath := c.String("container-binary-path")
	sourcePath := c.String("source")
	outputFlag := interact.GetOutputFormat()

	r := c.Root().Reader
	w := c.Root().Writer
	p := interact.NewPrinter(w)

	// Pack source manifests if --source is specified.
	var sourceManifests []byte
	if sourcePath != "" {
		fsys, pattern, err := resolveSourceFlag(sourcePath)
		if err != nil {
			return fmt.Errorf("failed to resolve source path: %w", err)
		}
		sourceManifests, err = archive.PackGlobFiles(fsys, pattern)
		if err != nil {
			return fmt.Errorf("failed to pack source manifests: %w", err)
		}
		slog.Info("Packed source manifests", "path", sourcePath, "size", len(sourceManifests))
	}

	// Step 1: Resolve device identity.
	deviceID, err := identity.GetDeviceID()
	if err != nil {
		return fmt.Errorf("failed to get device identity: %w", err)
	}
	slog.Info("Device identity", "device_id", deviceID)

	// Pre-flight: verify linux bridge binary exists when --connect is set.
	if connectFlag {
		if _, err := os.Stat(containerBinaryPath); err != nil {
			return fmt.Errorf("linux bridge binary not found at %s — install with: curl -fsSL https://github.com/vercel/bridge/releases/download/edge/install-edge.sh | sh", containerBinaryPath)
		}
	}

	kubeContext := currentKubeContext()

	// Single spinner reused across all phases, stored in context for child funcs.
	sp := interact.NewSpinner(w, "Connecting to bridge administrator...")
	ctx = interact.WithSpinner(ctx, sp)
	sp.Start(ctx)

	// Step 2: Connect to administrator.
	adm, err := connectAdmin(ctx, adminAddr)
	if err != nil {
		sp.Stop()
		return err
	}
	defer adm.Close()

	// Step 3: Check for existing bridges.
	// Skip duplicate detection when using --source without a deployment name,
	// since the server will determine the name from the manifests.
	listResp, err := adm.ListBridges(ctx, &bridgev1.ListBridgesRequest{DeviceId: deviceID, DeviceInfo: deviceInfo()})
	if err != nil {
		sp.Stop()
		return fmt.Errorf("failed to list bridges: %w", err)
	}

	// The expected bridge name: explicit --name flag, or default to workload name.
	if name == "" {
		name = deploymentName
	}

	if name != "" && !yes {
		for _, bridge := range listResp.Bridges {
			if bridge.Name == name {
				sp.Stop()
				p.Newline()
				p.Warn("An existing bridge already exists:")
				p.KeyValue("Name", bridge.Name)
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
				sp.Start(ctx)
				break
			}
		}
	}

	// Parse and validate server facade specs.
	facades, err := parseServerFacadeFlags(c.StringSlice("server-facade"))
	if err != nil {
		sp.Stop()
		return fmt.Errorf("invalid --server-facade: %w", err)
	}

	// Step 4: Create bridge.
	sp.SetTitle("Creating bridge server...")

	createResp, err := adm.CreateBridge(ctx, &bridgev1.CreateBridgeRequest{
		DeviceId:         deviceID,
		SourceDeployment: deploymentName,
		SourceNamespace:  sourceNamespace,
		Force:            yes,
		ProxyImage:       proxyImage,
		SourceManifests:  sourceManifests,
		ServerFacades:    facades,
		Name:             name,
		DeviceInfo:       deviceInfo(),
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

	// Step 5: Generate devcontainer config.
	baseConfig, err := devcontainer.ResolveConfigPath(c.String("devcontainer-config"))
	if err != nil {
		return err
	}
	// Use the user-specified listen port, or fall back to the PORT env var
	// from the source deployment, or 3000 as a last resort.
	appPort := c.Int("listen")
	if appPort == 0 {
		if portStr, ok := createResp.EnvVars["PORT"]; ok {
			if v, err := strconv.Atoi(portStr); err == nil && v > 0 {
				appPort = v
			}
		}
	}
	if appPort == 0 {
		appPort = 3000
	}
	interceptPort, err := netutil.FindFreePort()
	if err != nil {
		return fmt.Errorf("failed to allocate intercept server port: %w", err)
	}
	dcConfigPath, portMappings, err := generateDevcontainerConfig(p, baseConfig, featureRef, appPort, interceptPort, containerBinaryPath, deploymentName, createResp)
	if err != nil {
		return err
	}

	// Save local session so that exec can look up the config path by name.
	absDCConfigPath, _ := filepath.Abs(dcConfigPath)
	if err := session.Save(createResp.Name, absDCConfigPath); err != nil {
		slog.Warn("Failed to save session", "error", err)
	}

	if outputFlag == interact.OutputJSON && !connectFlag {
		ports := map[string]int32{
			"app":       int32(appPort),
			"intercept": int32(interceptPort),
		}
		for _, ap := range createResp.AppPorts {
			key := fmt.Sprintf("%d", ap)
			if _, exists := ports[key]; !exists {
				ports[key] = ap
			}
		}
		resp := &bridgev1.CreateCommandResponse{
			AppPort:                int32(appPort),
			DevcontainerConfigPath: absDCConfigPath,
			BridgeName:             createResp.Name,
			SourceDeployment:       deploymentName,
			Ports:                  ports,
		}
		return writeResult(w, resp, "")
	}

	if connectFlag {
		ct := container.NewDockerClient()
		labels := map[string]string{labelBridgeDeployment: createResp.Name}

		// Stop any existing container for this bridge so ports are released.
		ct.StopAll(ctx, container.StopAllOpts{Labels: labels})

		sp.Start(ctx)
		workspaceFolder, _ := filepath.Abs(filepath.Dir(filepath.Dir(filepath.Dir(dcConfigPath))))
		upClient := &devcontainer.CLIClient{
			WorkspaceFolder: workspaceFolder,
			ConfigPath:      dcConfigPath,
			Stdin:           r,
			Stdout:          w,
			Stderr:          c.Root().ErrWriter,
			UpArgs:          splitArgs(c.String("devcontainer-up-args")),
		}
		if err := startDevcontainer(ctx, w, ct, upClient, createResp.Name); err != nil {
			return err
		}

		printPortMappings(p, portMappings)

		dcErr := upClient.Exec(ctx, []string{"bash"})

		// Clean up the bridge when the user exits the devcontainer.
		sp.SetTitle("Removing bridge...")
		sp.Start(ctx)

		container.NewDockerClient().StopAll(ctx, container.StopAllOpts{
			Labels: map[string]string{labelBridgeDeployment: createResp.Name},
		})
		_, delErr := adm.DeleteBridge(ctx, &bridgev1.DeleteBridgeRequest{
			DeviceId:   deviceID,
			Name:       createResp.Name,
			Namespace:  createResp.Namespace,
			DeviceInfo: deviceInfo(),
		})
		sp.Stop()

		if delErr != nil {
			p.Warn(fmt.Sprintf("Failed to remove bridge: %v", delErr))
		} else {
			p.Success(fmt.Sprintf("Bridge %q removed", createResp.Name))
		}

		return dcErr
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
func generateDevcontainerConfig(p interact.Printer, baseConfigPath, featureRef string, appPort, interceptPort int, containerBinaryPath, deploymentName string, resp *bridgev1.CreateBridgeResponse) (string, []devcontainer.PortMapping, error) {
	dcName := resp.Name
	dcConfigPath := bridgeConfigPath(baseConfigPath, dcName)
	dcDir := filepath.Dir(dcConfigPath)

	// Load from base config, then overlay bridge settings.
	cfg, err := devcontainer.Load(baseConfigPath)
	if err != nil {
		return "", nil, fmt.Errorf("failed to load base devcontainer config: %w", err)
	}

	// Rebase relative build paths (dockerfile, context) so they resolve
	// correctly from the new config directory.
	cfg.RebaseBuildPaths(filepath.Dir(baseConfigPath), dcDir)

	cfg.Name = "bridge-" + dcName
	bridgeServerAddr := fmt.Sprintf("k8spf:///%s.%s:%d?workload=deployment", resp.DeploymentName, resp.Namespace, resp.Port)
	// Normalize "edge-<commit>" to "edge" so the feature install script
	// downloads from the correct GitHub release tag.
	featureVersion := Version
	if strings.HasPrefix(featureVersion, "edge-") {
		featureVersion = "edge"
	}
	featureOpts := map[string]any{
		"bridgeVersion":    featureVersion,
		"bridgeServerAddr": bridgeServerAddr,
		"forwardDomains":   "*",
		"appPort":          fmt.Sprintf("%d", appPort),
		"workspacePath":    "${containerWorkspaceFolder}",
	}
	if len(resp.VolumeMountPaths) > 0 {
		featureOpts["copyFiles"] = strings.Join(resp.VolumeMountPaths, ",")
	}
	cfg.SetFeature(featureRef, featureOpts)
	interceptAddr := fmt.Sprintf(":%d", interceptPort)
	// Remove any existing entry for the app port to avoid duplicates.
	filtered := cfg.AppPort[:0]
	for _, m := range cfg.AppPort {
		if m.ContainerPort != appPort {
			filtered = append(filtered, m)
		}
	}
	cfg.AppPort = append(filtered,
		devcontainer.PortMapping{HostPort: appPort, ContainerPort: appPort},
		devcontainer.PortMapping{HostPort: interceptPort, ContainerPort: interceptPort},
	)
	cfg.EnsureContainerEnv(meta.EnvInterceptorAddr, interceptAddr)
	cfg.EnsureCapAdd("NET_ADMIN")
	cfg.EnsureRunArgs("-l", labelBridgeDeployment+"="+dcName)
	cfg.EnsureContainerEnv("WORKLOAD_NAME", deploymentName)
	cfg.EnsureRemoteEnv("WORKLOAD_NAME", deploymentName)
	cfg.EnsureContainerEnv("BRIDGE_NAME", resp.Name)
	cfg.EnsureRemoteEnv("BRIDGE_NAME", resp.Name)

	if err := configureDevMounts(cfg, containerBinaryPath); err != nil {
		return "", nil, err
	}

	// Resolve appPort conflicts before saving.
	resolveAppPorts(cfg)

	if err := os.MkdirAll(dcDir, 0755); err != nil {
		return "", nil, fmt.Errorf("failed to create devcontainer directory: %w", err)
	}

	// Write source deployment env vars to a .env file so the devcontainer
	// gets them injected via --env-file.
	if len(resp.EnvVars) > 0 {
		envFilePath := filepath.Join(dcDir, "development.env")
		if err := writeEnvFile(envFilePath, resp.EnvVars); err != nil {
			return "", nil, fmt.Errorf("failed to write env file: %w", err)
		}
		cfg.EnsureRunArgs("--env-file", envFilePath)

		// The source pod's env vars may include AWS credentials that override
		// the developer's local ~/.aws config. Tell the intercept process to
		// unset them so k8s auth uses the mounted credentials instead.
		cfg.EnsureContainerEnv("BRIDGE_IGNORE_ENV_VARS",
			"AWS_ACCESS_KEY_ID,AWS_SECRET_ACCESS_KEY,AWS_ROLE_ARN,AWS_WEB_IDENTITY_TOKEN_FILE")
	}

	if err := cfg.Save(dcConfigPath); err != nil {
		return "", nil, fmt.Errorf("failed to write devcontainer config: %w", err)
	}

	// Ensure generated bridge config directories are gitignored.
	ensureGitignore(filepath.Dir(dcDir), "bridge-*/")

	p.Info(fmt.Sprintf("Devcontainer config written to %s", dcConfigPath))
	return dcConfigPath, cfg.AppPort, nil
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
// the linux bridge binary, KUBECONFIG, and Docker network access.
func configureDevMounts(cfg *devcontainer.Config, binaryPath string) error {
	// Bind-mount the linux bridge binary into the container,
	// unless the base config already provides one (e.g. in e2e tests).
	if !hasMountTarget(cfg, "/usr/local/bin/bridge") {
		binPath, err := filepath.Abs(binaryPath)
		if err != nil {
			return fmt.Errorf("failed to resolve bridge binary path: %w", err)
		}
		cfg.SetMount(fmt.Sprintf("source=%s,target=/usr/local/bin/bridge,type=bind,readonly", binPath))
	}

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
		// Docker --env-file treats everything after '=' as the literal value.
		// Do not add quotes — they would be included verbatim in the value.
		// Newlines are not supported; replace with spaces to avoid breaking the format.
		v = strings.ReplaceAll(v, "\n", " ")
		v = strings.ReplaceAll(v, "\r", "")
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

// defaultFeatureRef returns the devcontainer feature reference to use when
// neither --feature-ref nor BRIDGE_FEATURE_REF is set. In dev mode it
// prefers the local feature checkout; otherwise it falls back to the
// published ghcr.io image tagged with BridgeFeatureTag (or "latest").
func defaultFeatureRef() string {
	if Version == "dev" {
		p := filepath.Join(".devcontainer", devFeatureRef, "devcontainer-feature.json")
		if _, err := os.Stat(p); err == nil {
			return devFeatureRef
		}
	}
	if BridgeFeatureTag != "" {
		return featureRefBase + ":" + BridgeFeatureTag
	}
	return featureRefBase + ":latest"
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

// resolveAppPorts checks each appPort entry in the config and remaps the host
// port to the next free port if it's already in use. It tracks ports already
// claimed by earlier entries so that two appPorts requesting the same (or
// overlapping) host port never resolve to the same value.
func resolveAppPorts(cfg *devcontainer.Config) {
	claimed := make(map[int]struct{})
	for i := range cfg.AppPort {
		m := &cfg.AppPort[i]
		free, err := netutil.FindFreePortFrom(m.HostPort)
		if err != nil {
			continue
		}
		// If another entry already claimed this port, scan upward for the
		// next one that is both OS-free and unclaimed.
		for _, taken := claimed[free]; taken; _, taken = claimed[free] {
			next, err := netutil.FindFreePortFrom(free + 1)
			if err != nil {
				break
			}
			free = next
		}
		claimed[free] = struct{}{}
		m.HostPort = free
	}
}

// startDevcontainer builds and starts the devcontainer, then waits for the
// intercept process to become ready. Expects a Spinner in ctx via
// interact.WithSpinner.
func startDevcontainer(ctx context.Context, w io.Writer, ct container.Client, dcClient devcontainer.Client, bridgeName string) error {
	sp := interact.GetSpinner(ctx)
	logger := slog.With("bridge", bridgeName)

	labels := map[string]string{labelBridgeDeployment: bridgeName}

	slog.Debug("Starting devcontainer", "bridge", bridgeName)

	// Stop the spinner while the viewport streams build output.
	sp.Stop()

	// Start devcontainer and stream build output.
	upOpts := devcontainer.UpOpts{}
	if interact.IsJSON() {
		upOpts.LogFormat = devcontainer.LogFormatJSON
	}
	proc, err := dcClient.Up(ctx, upOpts)
	if err != nil {
		return fmt.Errorf("failed to start devcontainer: %w", err)
	}
	logger.Info("Devcontainer up.")

	// Tee the build output into the log so that `bridge debug` captures it.
	logWriter := &slogLineWriter{}
	buildOutput := io.TeeReader(proc.Output(), logWriter)

	vp := interact.NewViewport(w, interact.ViewportOpts{Title: "Building devcontainer..."})
	vp.Run(ctx, buildOutput)

	if err := proc.Wait(); err != nil {
		if output := logWriter.All(); output != "" {
			slog.Error("devcontainer up failed", "output", output)
			return fmt.Errorf("failed to start devcontainer: %w\n%s", err, logWriter.Tail(10))
		}
		return fmt.Errorf("failed to start devcontainer: %w", err)
	}
	vp.Clear()

	// Resume spinner for the connection phase.
	sp.SetTitle("Connecting container to proxy...")
	sp.Start(ctx)
	waitCtx, waitCancel := context.WithTimeout(ctx, time.Minute)
	defer waitCancel()
	containerID, err := container.WaitForID(waitCtx, ct, container.FindOpts{Labels: labels})
	if err != nil {
		sp.Stop()
		logger.ErrorContext(ctx, "Failed to start container", "err", err.Error())
		return err
	}

	readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readyCancel()
	if err := intercept.WaitForReady(readyCtx, ct, containerID); err != nil {
		sp.Stop()
		return fmt.Errorf("container failed to start: %w", err)
	}
	sp.Stop()
	return nil
}

// printPortMappings displays the resolved port mappings to the user.
func printPortMappings(p interact.Printer, mappings []devcontainer.PortMapping) {
	if len(mappings) == 0 {
		return
	}
	p.Newline()
	for _, m := range mappings {
		if m.HostPort == m.ContainerPort {
			p.Info(fmt.Sprintf("Port %d exposed", m.ContainerPort))
		} else {
			p.Info(fmt.Sprintf("Port %d → container:%d", m.HostPort, m.ContainerPort))
		}
	}
}

// splitArgs splits a space-separated string into individual arguments,
// returning nil for an empty string.
func splitArgs(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}

// resolveSourceFlag interprets the --source flag value and returns an fs.FS
// rooted at the appropriate directory plus a glob pattern for PackGlobFiles.
//
// Supported forms:
//   - Directory: -s _infra        → DirFS("_infra"), "*.yaml"
//   - Glob:      -s '_infra/*.yml' → DirFS("_infra"), "*.yml"
//   - File:      -s app.yaml      → DirFS("."),       "app.yaml"
func resolveSourceFlag(sourcePath string) (fs.FS, string, error) {
	if strings.ContainsAny(sourcePath, "*?[") {
		dir, pattern := filepath.Split(sourcePath)
		if dir == "" {
			dir = "."
		}
		return os.DirFS(dir), pattern, nil
	}

	info, err := os.Stat(sourcePath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to stat %q: %w", sourcePath, err)
	}

	if info.IsDir() {
		return os.DirFS(sourcePath), "*.y*ml", nil
	}

	dir := filepath.Dir(sourcePath)
	if dir == "" {
		dir = "."
	}
	return os.DirFS(dir), filepath.Base(sourcePath), nil
}

// slogLineWriter is an io.Writer that logs each complete line via slog.Debug
// and retains all lines. Tail returns the last N lines for error messages.
type slogLineWriter struct {
	buf   []byte
	lines []string
}

func (w *slogLineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:idx]), "\r")
		w.buf = w.buf[idx+1:]
		if line != "" {
			slog.Debug("devcontainer up", "output", line)
			w.lines = append(w.lines, line)
		}
	}
	return len(p), nil
}

// All returns every captured line joined by newlines.
func (w *slogLineWriter) All() string {
	return strings.Join(w.lines, "\n")
}

// Tail returns the last n captured lines joined by newlines.
func (w *slogLineWriter) Tail(n int) string {
	if len(w.lines) <= n {
		return strings.Join(w.lines, "\n")
	}
	return strings.Join(w.lines[len(w.lines)-n:], "\n")
}

// bridgeConfigPath returns the expected devcontainer config path for a bridge,
// given the base config path and bridge deployment name. The bridge config is
// always placed as a subdirectory of the nearest .devcontainer ancestor. If no
// .devcontainer directory exists in the path, one is created next to the base
// config.
func bridgeConfigPath(baseConfigPath, bridgeName string) string {
	bridgeDir := fmt.Sprintf("bridge-%s", bridgeName)
	dir := filepath.Dir(baseConfigPath)
	for dir != "." && dir != "/" {
		if filepath.Base(dir) == ".devcontainer" {
			return filepath.Join(dir, bridgeDir, "devcontainer.json")
		}
		dir = filepath.Dir(dir)
	}
	return filepath.Join(filepath.Dir(baseConfigPath), ".devcontainer", bridgeDir, "devcontainer.json")
}

// parseServerFacadeFlags parses and validates --server-facade flag values (JSON
// strings or file paths) into ServerFacade proto messages.
func parseServerFacadeFlags(vals []string) ([]*bridgev1.ServerFacade, error) {
	v, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("init validator: %w", err)
	}

	var facades []*bridgev1.ServerFacade
	for _, val := range vals {
		data := []byte(val)
		if !strings.HasPrefix(strings.TrimSpace(val), "{") {
			fileData, err := os.ReadFile(val)
			if err != nil {
				return nil, fmt.Errorf("read server facade file %q: %w", val, err)
			}
			data = fileData
		}
		var f bridgev1.ServerFacade
		if err := protojson.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("parse server facade spec: %w", err)
		}
		if err := v.Validate(&f); err != nil {
			return nil, fmt.Errorf("invalid server facade spec: %w", err)
		}
		facades = append(facades, &f)
	}
	return facades, nil
}
