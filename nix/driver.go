package nix

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/drivers/shared/executor"
	"github.com/hashicorp/nomad/helper/pluginutils/hclutils"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	driversUtil "github.com/hashicorp/nomad/plugins/drivers/utils"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	"github.com/hashicorp/nomad/plugins/shared/structs"
)

const (
	// pluginName is the name of the plugin
	pluginName = "nix"

	// fingerprintPeriod is the interval at which the driver will send fingerprint responses
	fingerprintPeriod = 30 * time.Second

	// taskHandleVersion is the version of task handle which this driver sets
	// and understands how to decode driver state
	taskHandleVersion = 1

	// startup timeouts
	machinePropertiesTimeout = 30 * time.Second
	machineAddressTimeout    = 30 * time.Second
)

var (
	// populated by Makefile
	pluginVersion = "0.1.0"

	// pluginInfo is the response returned for the PluginInfo RPC
	pluginInfo = &base.PluginInfoResponse{
		Type:              base.PluginTypeDriver,
		PluginApiVersions: []string{drivers.ApiVersion010},
		PluginVersion:     pluginVersion,
		Name:              pluginName,
	}

	// configSpec is the hcl specification returned by the ConfigSchema RPC
	configSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"enabled": hclspec.NewDefault(
			hclspec.NewAttr("enabled", "bool", false),
			hclspec.NewLiteral("true"),
		),
		"volumes": hclspec.NewDefault(
			hclspec.NewAttr("volumes", "bool", false),
			hclspec.NewLiteral("true"),
		),
	})

	// taskConfigSpec is the hcl specification for the driver config section of
	// a task within a job. It is returned in the TaskConfigSchema RPC
	taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"boot": hclspec.NewDefault(
			hclspec.NewAttr("boot", "bool", false),
			hclspec.NewLiteral("false"),
		),
		"ephemeral": hclspec.NewAttr("ephemeral", "bool", false),
		"network_veth": hclspec.NewDefault(
			hclspec.NewAttr("network_veth", "bool", false),
			hclspec.NewLiteral("false"),
		),
		"process_two": hclspec.NewDefault(
			hclspec.NewAttr("process_two", "bool", false),
			hclspec.NewLiteral("false"),
		),
		"read_only": hclspec.NewAttr("read_only", "bool", false),
		"user_namespacing": hclspec.NewDefault(
			hclspec.NewAttr("user_namespacing", "bool", false),
			hclspec.NewLiteral("false"),
		),
		"command": hclspec.NewAttr("command", "list(string)", false),
		"console": hclspec.NewDefault(
			hclspec.NewAttr("console", "string", false),
			hclspec.NewLiteral(`"read-only"`),
		),
		"image": hclspec.NewAttr("image", "string", false),
		"image_download": hclspec.NewBlock("image_download", false,
			hclspec.NewObject(map[string]*hclspec.Spec{
				"url": hclspec.NewAttr("url", "string", true),
				"type": hclspec.NewDefault(
					hclspec.NewAttr("type", "string", false),
					hclspec.NewLiteral(`"tar"`),
				),
				"force": hclspec.NewDefault(
					hclspec.NewAttr("force", "bool", false),
					hclspec.NewLiteral("false"),
				),
				"verify": hclspec.NewDefault(
					hclspec.NewAttr("verify", "string", false),
					hclspec.NewLiteral(`"no"`),
				),
			})),
		// "machine":           hclspec.NewAttr("machine", "string", false),
		"pivot_root": hclspec.NewAttr("pivot_root", "string", false),
		"resolv_conf": hclspec.NewDefault(
			hclspec.NewAttr("resolv_conf", "string", false),
			hclspec.NewLiteral(`"copy-host"`),
		),
		"user":              hclspec.NewAttr("user", "string", false),
		"volatile":          hclspec.NewAttr("volatile", "string", false),
		"working_directory": hclspec.NewAttr("working_directory", "string", false),
		"bind":              hclspec.NewAttr("bind", "list(map(string))", false),
		"bind_read_only":    hclspec.NewAttr("bind_read_only", "list(map(string))", false),
		"environment":       hclspec.NewAttr("environment", "list(map(string))", false),
		"port_map":          hclspec.NewAttr("port_map", "list(map(number))", false),
		"ports":             hclspec.NewAttr("ports", "list(string)", false),
		"capability":        hclspec.NewAttr("capability", "list(string)", false),
		"network_zone":      hclspec.NewAttr("network_zone", "string", false),
		"link_journal":      hclspec.NewAttr("link_journal", "string", false),
		"nixos":             hclspec.NewAttr("nixos", "string", false),
		"packages":          hclspec.NewAttr("packages", "list(string)", false),
		"sanitize_names":    hclspec.NewAttr("sanitize_names", "bool", false),
	})

	// capabilities is returned by the Capabilities RPC and indicates what
	// optional features this driver supports
	capabilities = &drivers.Capabilities{
		SendSignals: true,
		Exec:        true,
		FSIsolation: drivers.FSIsolationImage,
		NetIsolationModes: []drivers.NetIsolationMode{
			drivers.NetIsolationModeHost,
			drivers.NetIsolationModeGroup,
		},
		MountConfigs: drivers.MountConfigSupportAll,
	}
)

// Driver is a driver for running nspawn containers
type Driver struct {
	// eventer is used to handle multiplexing of TaskEvents calls such that an
	// event can be broadcast to all callers
	eventer *eventer.Eventer

	// config is the driver configuration set by the SetConfig RPC
	config *Config

	// nomadConfig is the client config from nomad
	nomadConfig *base.ClientDriverConfig

	// tasks is the in memory datastore mapping taskIDs to rawExecDriverHandles
	tasks *taskStore

	// ctx is the context for the driver. It is passed to other subsystems to
	// coordinate shutdown
	ctx context.Context

	// signalShutdown is called when the driver is shutting down and cancels the
	// ctx passed to any subsystems
	signalShutdown context.CancelFunc

	// logger will log to the Nomad agent
	logger hclog.Logger

	// Receives OOM events
	oomChan     chan *OOM
	oomListener *OOMListener
}

// Config is the driver configuration set by the SetConfig RPC call
type Config struct {
	// Enabled is set to true to enable the nspawn driver
	Enabled bool `codec:"enabled"`
	Volumes bool `codec:"volumes"`
}

// TaskState is the state which is encoded in the handle returned in
// StartTask. This information is needed to rebuild the task state and handler
// during recovery.
type TaskState struct {
	ReattachConfig *structs.ReattachConfig
	MachineName    string
	StartedAt      time.Time
}

// NewPlugin returns a new nspawn driver object
func NewPlugin(logger hclog.Logger, oomListener *OOMListener) drivers.DriverPlugin {
	ctx, cancel := context.WithCancel(context.Background())
	logger = logger.Named(pluginName)

	return &Driver{
		eventer: eventer.NewEventer(ctx, logger),
		config: &Config{
			Enabled: true,
			Volumes: true,
		},
		tasks:          newTaskStore(),
		ctx:            ctx,
		signalShutdown: cancel,
		logger:         logger,
		oomListener:    oomListener,
	}
}

func (d *Driver) TaskConfigSchema() (*hclspec.Spec, error) {
	return taskConfigSpec, nil
}

func (d *Driver) Capabilities() (*drivers.Capabilities, error) {
	return capabilities, nil
}

func (d *Driver) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint)
	go d.handleFingerprint(ctx, ch)
	return ch, nil
}

func (d *Driver) handleFingerprint(ctx context.Context, ch chan<- *drivers.Fingerprint) {
	defer close(ch)
	ticker := time.NewTimer(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			ticker.Reset(fingerprintPeriod)
			ch <- d.buildFingerprint()
		}
	}
}

// buildFingerprint returns the driver's fingerprint data
func (d *Driver) buildFingerprint() *drivers.Fingerprint {
	fp := &drivers.Fingerprint{
		Attributes:        map[string]*structs.Attribute{},
		Health:            drivers.HealthStateUndetected,
		HealthDescription: "disabled",
	}

	if !d.config.Enabled {
		return fp
	}

	err := isInstalled()
	version, vErr := systemdVersion()

	if err == nil && vErr == nil && driversUtil.IsUnixRoot() {
		fp.Health = drivers.HealthStateHealthy
		fp.HealthDescription = "ready"
		fp.Attributes["driver.nix"] = structs.NewBoolAttribute(true)
		fp.Attributes["driver.nix.nspawn.version"] = structs.NewStringAttribute(version)
		fp.Attributes["driver.nix.volumes"] = structs.NewBoolAttribute(d.config.Volumes)
	}

	return fp
}

func (d *Driver) RecoverTask(handle *drivers.TaskHandle) error {
	d.logger.Debug("RecoverTask called")
	if handle == nil {
		return fmt.Errorf("error: handle cannot be nil")
	}

	if _, ok := d.tasks.Get(handle.Config.ID); ok {
		return nil
	}

	var taskState TaskState
	if err := handle.GetDriverState(&taskState); err != nil {
		return fmt.Errorf("failed to decode task state from handle: %v", err)
	}

	plugRC, err := structs.ReattachConfigToGoPlugin(taskState.ReattachConfig)
	if err != nil {
		return fmt.Errorf("failed to build ReattachConfig from taskConfig state: %v", err)
	}

	execImpl, pluginClient, err := executor.ReattachToExecutor(plugRC, d.logger)
	if err != nil {
		return fmt.Errorf("failed to reattach to executor: %v", err)
	}

	p, e := DescribeMachine(taskState.MachineName, machinePropertiesTimeout)
	if e != nil {
		d.logger.Error("failed to get machine information", "error", e)
		return e
	}

	netIF, e := p.GetNetworkInterfaces()
	if e != nil {
		d.logger.Error("failed to get machine network interfacves", "error", err)
	}

	h := &taskHandle{
		machine:           p,
		logger:            d.logger,
		networkInterfaces: netIF,

		exec:         execImpl,
		pluginClient: pluginClient,
		taskConfig:   handle.Config,
		procState:    drivers.TaskStateRunning,
		startedAt:    taskState.StartedAt,
	}

	d.tasks.Set(handle.Config.ID, h)

	go h.run()

	return nil
}

var sanitizeName = regexp.MustCompile("[^a-zA-Z0-9-]+")

func (d *Driver) StartTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	d.logger.Debug("StartTask called")
	if _, ok := d.tasks.Get(cfg.ID); ok {
		return nil, nil, fmt.Errorf("task with ID %q already started", cfg.ID)
	}

	var driverConfig MachineConfig
	if err := cfg.DecodeDriverConfig(&driverConfig); err != nil {
		return nil, nil, fmt.Errorf("failed to decode driver config: %v", err)
	}
	if driverConfig.SanitizeNames == nil {
		t := true
		driverConfig.SanitizeNames = &t
	}

	handle := drivers.NewTaskHandle(taskHandleVersion)
	handle.Config = cfg

	if *driverConfig.SanitizeNames {
		saneName := sanitizeName.ReplaceAllString(cfg.Name, "-")
		cut := len(saneName)
		if cut > 27 {
			cut = 27
		}
		driverConfig.Machine = saneName[0:cut] + "-" + cfg.AllocID
	} else {
		driverConfig.Machine = cfg.Name + "-" + cfg.AllocID
	}

	d.oomChan = d.oomListener.Register(driverConfig.Machine)

	driverConfig.Port = make(map[string]string)

	//If network isolation is enabled, disable user namespacing and network-veth
	if cfg.NetworkIsolation != nil {
		driverConfig.NetworkNamespace = cfg.NetworkIsolation.Path
		driverConfig.UserNamespacing = false
		driverConfig.NetworkVeth = false
	}
	// pass predefined environment vars
	if driverConfig.Environment == nil {
		driverConfig.Environment = make(hclutils.MapStrStr)
	}
	for k, v := range cfg.Env {
		driverConfig.Environment[k] = v
	}

	for k, v := range driverConfig.Environment {
		if strings.Contains(k, "-") {
			delete(driverConfig.Environment, k)
			driverConfig.Environment[strings.ReplaceAll(k, "-", "_")] = v
		}
	}

	// bind Task Directories into container
	taskDirs := cfg.TaskDir()
	if driverConfig.Bind == nil {
		driverConfig.Bind = make(hclutils.MapStrStr)
	}
	driverConfig.Bind[taskDirs.SharedAllocDir] = cfg.Env["NOMAD_ALLOC_DIR"]
	driverConfig.Bind[taskDirs.LocalDir] = cfg.Env["NOMAD_TASK_DIR"]
	driverConfig.Bind[taskDirs.SecretsDir] = cfg.Env["NOMAD_SECRETS_DIR"]

	//bind volumes into container
	if cfg.Mounts != nil && len(cfg.Mounts) > 0 {
		if !d.config.Volumes {
			d.logger.Error("volumes are not enabled; cannot mount host paths")
			return nil, nil, fmt.Errorf("volumes are not enabled; cannot mount host paths")
		}
		if driverConfig.BindReadOnly == nil {
			driverConfig.BindReadOnly = make(hclutils.MapStrStr)
		}
		for _, m := range cfg.Mounts {
			if m.Readonly {
				driverConfig.BindReadOnly[m.HostPath] = m.TaskPath
			} else {
				driverConfig.Bind[m.HostPath] = m.TaskPath
			}
		}
	}

	if driverConfig.NixOS != "" {
		d.eventer.EmitEvent(&drivers.TaskEvent{
			TaskID:    cfg.ID,
			AllocID:   cfg.AllocID,
			TaskName:  cfg.Name,
			Timestamp: time.Now(),
			Message:   "Building NixOS",
			Annotations: map[string]string{
				"nixos": driverConfig.NixOS,
			},
		})

		if err := driverConfig.prepareNixOS(taskDirs.Dir); err != nil {
			return nil, nil, err
		}
	}

	if len(driverConfig.NixPackages) > 0 {
		d.eventer.EmitEvent(&drivers.TaskEvent{
			TaskID:    cfg.ID,
			AllocID:   cfg.AllocID,
			TaskName:  cfg.Name,
			Timestamp: time.Now(),
			Message:   "Building Nix Packages",
			Annotations: map[string]string{
				"packages": strings.Join(driverConfig.NixPackages, " "),
			},
		})

		if err := driverConfig.prepareNixPackages(taskDirs.Dir); err != nil {
			return nil, nil, err
		}
	}

	if driverConfig.Properties == nil {
		driverConfig.Properties = make(hclutils.MapStrStr)
	}

	if cfg.Resources.NomadResources != nil {
		if cfg.Resources.NomadResources.Memory.MemoryMaxMB != 0 {
			driverConfig.Properties["MemoryHigh"] = strconv.Itoa(int(cfg.Resources.NomadResources.Memory.MemoryMB * 1024 * 1024))
			driverConfig.Properties["MemoryMax"] = strconv.Itoa(int(cfg.Resources.NomadResources.Memory.MemoryMaxMB * 1024 * 1024))
		} else {
			driverConfig.Properties["MemoryMax"] = strconv.Itoa(int(cfg.Resources.NomadResources.Memory.MemoryMB * 1024 * 1024))
		}
	}

	// Setup port mapping and exposed ports
	if cfg.Resources != nil {
		if len(driverConfig.PortMap) > 0 && len(driverConfig.Ports) > 0 {
			d.logger.Error("Invalid port declaration; use of port_map and ports")
			return nil, nil, fmt.Errorf("Invalid port declaration; use of port_map and ports")
		}

		if len(driverConfig.PortMap) > 0 && len(cfg.Resources.NomadResources.Networks) == 0 {
			d.logger.Error("Trying to map ports but no network interface is available")
			return nil, nil, fmt.Errorf("Trying to map ports but no network interface is available")
		}

		if len(driverConfig.Ports) > 0 && cfg.Resources.Ports == nil {
			d.logger.Error("No ports defined in network stanza")
			return nil, nil, fmt.Errorf("No ports defined in network stanza")
		}

		if len(driverConfig.Ports) > 0 {
			for _, port := range driverConfig.Ports {
				p, ok := cfg.Resources.Ports.Get(port)
				if !ok {
					d.logger.Error("Port " + port + " not found, check network stanza")
					return nil, nil, fmt.Errorf("Port %q not found, check network stanza", port)
				}
				to := p.To
				if to == 0 {
					to = p.Value
				}
				driverConfig.Port[port] = fmt.Sprintf("%d:%d", p.Value, to)
				d.logger.Debug("exposed port", "port", p.Value, "to", to)
			}
		} else if len(driverConfig.PortMap) > 0 {
			network := cfg.Resources.NomadResources.Networks[0]
			for _, port := range network.ReservedPorts {
				// By default we will map the allocated port 1:1 to the container
				machinePort := port.Value

				// If the user has mapped a port using port_map we'll change it here
				if mapped, ok := driverConfig.PortMap[port.Label]; ok {
					machinePort = mapped
				}

				hostPort := port.Value
				driverConfig.Port[port.Label] = fmt.Sprintf("%d:%d", hostPort, machinePort)

				d.logger.Debug("allocated static port", "ip", network.IP, "port", hostPort)
				d.logger.Debug("exposed port", "port", machinePort)
			}

			for _, port := range network.DynamicPorts {
				// By default we will map the allocated port 1:1 to the container
				machinePort := port.Value

				// If the user has mapped a port using port_map we'll change it here
				if mapped, ok := driverConfig.PortMap[port.Label]; ok {
					machinePort = mapped
				}

				hostPort := port.Value
				driverConfig.Port[port.Label] = fmt.Sprintf("%d:%d", hostPort, machinePort)

				d.logger.Debug("allocated mapped port", "ip", network.IP, "port", hostPort)
				d.logger.Debug("exposed port", "port", machinePort)
			}

		}
	}

	// Validate config
	if err := driverConfig.Validate(); err != nil {
		return nil, nil, fmt.Errorf("failed to validate task config: %v", err)
	}

	// Download image
	if driverConfig.ImageDownload != nil {
		d.eventer.EmitEvent(&drivers.TaskEvent{
			TaskID:    cfg.ID,
			AllocID:   cfg.AllocID,
			TaskName:  cfg.Name,
			Timestamp: time.Now(),
			Message:   "Downloading image",
			Annotations: map[string]string{
				"image": driverConfig.Image,
				"url":   driverConfig.ImageDownload.URL,
			},
		})
		err := DownloadImage(driverConfig.ImageDownload.URL,
			driverConfig.Image, driverConfig.ImageDownload.Verify,
			driverConfig.ImageDownload.Type,
			driverConfig.ImageDownload.Force, d.logger)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to download image: %v", err)
		}
	}

	// Gather image path
	imagePath, err := driverConfig.GetImagePath()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to gather image path: %v", err)
	}

	driverConfig.imagePath = imagePath

	// Get nspawn arguments
	args, err := driverConfig.ConfigArray()
	if err != nil {
		d.logger.Error("Error generating machine config", "error", err)
		return nil, nil, err
	}

	d.logger.Debug("starting nspawn task", "driver_cfg", hclog.Fmt("%+v", driverConfig))
	d.logger.Debug("resources", "nomad", fmt.Sprintf("%+v", cfg.Resources.NomadResources), "linux", fmt.Sprintf("%+v", cfg.Resources.LinuxResources), "ports", fmt.Sprintf("%+v", cfg.Resources.Ports))
	d.logger.Debug("commad arguments", "args", args)

	executorConfig := &executor.ExecutorConfig{
		LogFile:  filepath.Join(cfg.TaskDir().Dir, "executor.out"),
		LogLevel: "debug",
	}

	exec, pluginClient, err := executor.CreateExecutor(d.logger, d.nomadConfig, executorConfig)

	if err != nil {
		return nil, nil, fmt.Errorf("failed to create executor: %v", err)
	}

	execCmd := &executor.ExecCommand{
		Cmd:        "systemd-nspawn",
		Args:       args,
		StdoutPath: cfg.StdoutPath,
		StderrPath: cfg.StderrPath,
		Resources:  cfg.Resources,
	}

	ps, err := exec.Launch(execCmd)
	if err != nil {
		pluginClient.Kill()
		return nil, nil, fmt.Errorf("failed to launch command with executor: %v", err)
	}

	printErr := func() {
		logDir := cfg.TaskDir().LogDir
		logs, err := filepath.Glob(filepath.Join(logDir, cfg.Name+"*"))
		if err != nil {
			d.logger.Error("error finding log files", err)
			return
		}

		for _, l := range logs {
			out, err := ioutil.ReadFile(l)
			if err != nil {
				continue
			}
			lines := strings.Split(strings.Trim(string(out), "\n"), "\n")
			// Continue if there's no output
			if len(lines) == 0 || len(lines[len(lines)-1]) == 0 {
				continue
			}
			d.logger.Error("systemd-nspawn failed", "file", filepath.Base(l), "out", lines[len(lines)-1])
			d.eventer.EmitEvent(&drivers.TaskEvent{
				TaskID:    cfg.ID,
				AllocID:   cfg.AllocID,
				TaskName:  cfg.Name,
				Timestamp: time.Now(),
				Message:   lines[len(lines)-1],
				Err:       fmt.Errorf("Systemd-Nspawn failed"),
			})
		}
	}

	p, err := DescribeMachine(driverConfig.Machine, machinePropertiesTimeout)
	if err != nil {
		d.logger.Error("failed to get machine information", "error", err)
		if ps.ExitCode != 0 {
			printErr()
			err = fmt.Errorf("systemd-nspawn failed to start task")
		}
		if !pluginClient.Exited() {
			if err := exec.Shutdown("", 0); err != nil {
				d.logger.Error("destroying executor failed", "err", err)
			}

			pluginClient.Kill()
		}
		return nil, nil, err
	}
	d.logger.Debug("gathered information about new machine", "name", p.Name, "leader", p.Leader)

	var ip string
	netIF := []string{}
	if len(p.NetworkInterfaces) > 0 {
		addr, err := MachineAddresses(driverConfig.Machine, machineAddressTimeout)
		if err != nil {
			d.logger.Error("failed to get machine addresses", "error", err, "addresses", addr)
			if ps.ExitCode != 0 {
				printErr()
				err = fmt.Errorf("systemd-nspawn failed to start task")
			}
			if !pluginClient.Exited() {
				if err := exec.Shutdown("", 0); err != nil {
					d.logger.Error("destroying executor failed", "err", err)
				}

				pluginClient.Kill()
			}
			return nil, nil, err
		}

		d.logger.Debug("gathered address of new machine", "name", p.Name, "ip", addr.IPv4.String())
		ip = addr.IPv4.String()

		netIF, err = p.GetNetworkInterfaces()
		if err != nil {
			d.logger.Error("failed to get machine network interfacves", "error", err)
		}
	} else if len(cfg.Resources.NomadResources.Networks) > 0 {
		ip = cfg.Resources.NomadResources.Networks[0].IP
	}

	network := &drivers.DriverNetwork{
		PortMap:       driverConfig.PortMap,
		IP:            ip,
		AutoAdvertise: false,
	}

	if cfg.NetworkIsolation == nil && len(p.NetworkInterfaces) > 0 {
		err = ConfigureIPTablesRules(false, netIF)
		if err != nil {
			d.logger.Error("Failed to set up IPTables rules", "error", err)
		}
	}

	h := &taskHandle{
		machine:           p,
		logger:            d.logger,
		networkInterfaces: netIF,

		exec:         exec,
		pluginClient: pluginClient,
		taskConfig:   cfg,
		procState:    drivers.TaskStateRunning,
		startedAt:    time.Now().Round(time.Millisecond),
	}

	driverState := TaskState{
		ReattachConfig: structs.ReattachConfigFromGoPlugin(pluginClient.ReattachConfig()),
		MachineName:    driverConfig.Machine,
		StartedAt:      h.startedAt,
	}

	if err := handle.SetDriverState(&driverState); err != nil {
		d.logger.Error("failed to start task, error setting driver state", "error", err)
		return nil, nil, fmt.Errorf("failed to set driver state: %v", err)
	}

	d.tasks.Set(cfg.ID, h)

	go h.run()

	return handle, network, nil
}

func (d *Driver) WaitTask(ctx context.Context, taskID string) (<-chan *drivers.ExitResult, error) {
	d.logger.Debug("WaitTask called")
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	ch := make(chan *drivers.ExitResult)
	go d.handleWait(ctx, handle, ch)

	return ch, nil
}

func (d *Driver) handleWait(ctx context.Context, handle *taskHandle, ch chan *drivers.ExitResult) {
	defer close(ch)
	var result *drivers.ExitResult

	ps, err := handle.exec.Wait(ctx)
	if err != nil {
		result = &drivers.ExitResult{
			Err: fmt.Errorf("executor: error waiting on process: %v", err),
		}
	} else {
		result = &drivers.ExitResult{
			ExitCode: ps.ExitCode,
			Signal:   ps.Signal,
		}
	}

	// logs about OOM may take a bit to show up.
	select {
	case <-time.After(5 * time.Second):
	case <-d.oomChan:
		result.OOMKilled = true
		result.Err = fmt.Errorf("Out of memory")
	}

	d.oomListener.Deregister(handle.machine.Name)

	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case ch <- result:
		}
	}
}

func (d *Driver) StopTask(taskID string, timeout time.Duration, signal string) error {
	d.logger.Debug("StopTask called")
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	if handle.taskConfig.NetworkIsolation == nil && len(handle.networkInterfaces) > 0 &&
		!strings.HasPrefix(handle.networkInterfaces[0], "vz-") {
		if err := ConfigureIPTablesRules(true, handle.networkInterfaces); err != nil {
			d.logger.Error("StopTask: Failed to remove IPTables rules", "error", err)
		}
	}

	if err := handle.exec.Shutdown(signal, timeout); err != nil {
		if handle.pluginClient.Exited() {
			return nil
		}
		return fmt.Errorf("StopTask: executor Shutdown failed: %v", err)
	}

	return nil
}

func (d *Driver) DestroyTask(taskID string, force bool) error {
	d.logger.Debug("DestroyTask called")
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	if handle.IsRunning() && !force {
		return fmt.Errorf("cannot destroy running task")
	}

	if !handle.pluginClient.Exited() {
		handle.pluginClient.Kill()
	}

	d.tasks.Delete(taskID)
	return nil
}

func (d *Driver) InspectTask(taskID string) (*drivers.TaskStatus, error) {
	d.logger.Debug("InspectTask called")
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	return handle.TaskStatus(), nil
}

func (d *Driver) TaskStats(ctx context.Context, taskID string, interval time.Duration) (<-chan *drivers.TaskResourceUsage, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	return handle.exec.Stats(ctx, interval)
}

func (d *Driver) TaskEvents(ctx context.Context) (<-chan *drivers.TaskEvent, error) {
	return d.eventer.TaskEvents(ctx)
}

func (d *Driver) SignalTask(taskID string, signal string) error {
	d.logger.Debug("SignalTask called")
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}
	sig := os.Interrupt
	if s, ok := SignalLookup[signal]; ok {
		sig = s
	} else {
		d.logger.Warn("unknown signal to send to task, using SIGINT instead", "signal", signal, "task_id", handle.taskConfig.ID)

	}
	return handle.exec.Signal(sig)
}

// var _ drivers.ExecTaskStreamingDriver = (*Driver)(nil)
var _ drivers.ExecTaskStreamingRawDriver = (*Driver)(nil)

func (d *Driver) ExecTaskStreamingRaw(ctx context.Context,
	taskID string,
	command []string,
	tty bool,
	stream drivers.ExecTaskStream) error {

	if len(command) == 0 {
		return fmt.Errorf("error cmd must have at least one value")
	}
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	leader := handle.machine.Leader

	environ, err := os.Open(fmt.Sprintf("/proc/%d/environ", leader))
	if err != nil {
		return err
	}
	defer environ.Close()

	s := bufio.NewScanner(environ)
	s.Scan()

	cmd := []string{
		"nsenter",
		"--target", strconv.FormatInt(int64(leader), 10),
		"--all", "/bin/env", "-i", "-",
	}

	for name, value := range readEnviron(leader) {
		cmd = append(cmd, name+"="+value)
	}

	if err := execSupported(handle); err != nil {
		return err
	}

	cmd = append(cmd, command...)

	return handle.exec.ExecStreaming(ctx, cmd, tty, stream)
}

func (d *Driver) ExecTask(taskID string, cmd []string, timeout time.Duration) (*drivers.ExecTaskResult, error) {
	if len(cmd) == 0 {
		return nil, fmt.Errorf("error cmd must have at least one value")
	}
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	if err := execSupported(handle); err != nil {
		return nil, err
	}

	command := []string{"systemd-run", "--wait", "--service-type=exec",
		"--collect", "--quiet", "--machine", handle.machine.Name, "--pipe"}
	command = append(command, cmd...)

	out, exitCode, err := handle.exec.Exec(time.Now().Add(timeout), command[0], command[1:])
	if err != nil {
		return nil, err
	}

	return &drivers.ExecTaskResult{
		Stdout: out,
		ExitResult: &drivers.ExitResult{
			ExitCode: exitCode,
		},
	}, nil
}

// execSupported checks if container was stared with boot parameter, otherwise
// systemd-run does not work
func execSupported(handle *taskHandle) error {
	var driverConfig MachineConfig
	if err := handle.taskConfig.DecodeDriverConfig(&driverConfig); err != nil {
		return fmt.Errorf("failed to decode driver config: %v", err)
	}
	// if !driverConfig.Boot {
	// 	return fmt.Errorf("cannot exec command in task started without boot parameter")
	// }
	return nil
}

func (d *Driver) PluginInfo() (*base.PluginInfoResponse, error) {
	return pluginInfo, nil
}

func (d *Driver) ConfigSchema() (*hclspec.Spec, error) {
	return configSpec, nil
}

func (d *Driver) SetConfig(cfg *base.Config) error {
	var config Config
	if len(cfg.PluginConfig) != 0 {
		if err := base.MsgPackDecode(cfg.PluginConfig, &config); err != nil {
			return err
		}
	}

	d.config = &config
	if cfg.AgentConfig != nil {
		d.nomadConfig = cfg.AgentConfig.Driver
	}

	return nil
}

func (d *Driver) Shutdown(ctx context.Context) error {
	d.signalShutdown()
	return nil
}
