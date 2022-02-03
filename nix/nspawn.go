package nix

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/go-iptables/iptables"
	systemdDbus "github.com/coreos/go-systemd/dbus"
	"github.com/coreos/go-systemd/import1"
	"github.com/coreos/go-systemd/machine1"
	systemdUtil "github.com/coreos/go-systemd/util"
	"github.com/godbus/dbus"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/helper/pluginutils/hclutils"
)

const (
	machineMonitorIntv = 2 * time.Second
	dbusInterface      = "org.freedesktop.machine1.Manager"
	dbusPath           = "/org/freedesktop/machine1"

	TarImage string = "tar"
	RawImage string = "raw"

	closureNix = `
{ flakes }:
let
  nixpkgs = builtins.getFlake "github:nixos/nixpkgs/nixos-21.05";
  inherit (nixpkgs.legacyPackages.x86_64-linux) lib buildPackages;
  inherit (builtins) match elemAt getFlake length fromJSON;

  resolve = flakeURI:
    let
      split = match "^([^#]+)#(.*)$" flakeURI;
      flake = elemAt split 0;
      attr = elemAt split 1;
      path = lib.splitString "." attr;
      root = getFlake flake;

      paths = [
        path
        ([ "packages" "x86_64-linux" ] ++ path)
        ([ "legacyPackages" "x86_64-linux" ] ++ path)
      ];

      findRoute = route:
        if (lib.hasAttrByPath route root) then
          lib.getAttrFromPath route root
        else
          null;

      notNull = e: e != null;
      allFound = lib.filter notNull (map findRoute paths);
    in if (length allFound) > 0 then
      elemAt allFound 0
    else
      throw "No attribute '${attr}' in flake '${flake}' found";

  drvs = map resolve (fromJSON flakes);
in buildPackages.closureInfo { rootPaths = drvs; }
`
)

var (
	transferMut sync.Mutex
	mutMap      = make(map[string]*sync.Mutex)
)

var SignalLookup = map[string]os.Signal{
	"SIGABRT":  syscall.SIGABRT,
	"SIGALRM":  syscall.SIGALRM,
	"SIGBUS":   syscall.SIGBUS,
	"SIGCHLD":  syscall.SIGCHLD,
	"SIGCONT":  syscall.SIGCONT,
	"SIGFPE":   syscall.SIGFPE,
	"SIGHUP":   syscall.SIGHUP,
	"SIGILL":   syscall.SIGILL,
	"SIGINT":   syscall.SIGINT,
	"SIGIO":    syscall.SIGIO,
	"SIGIOT":   syscall.SIGIOT,
	"SIGKILL":  syscall.SIGKILL,
	"SIGPIPE":  syscall.SIGPIPE,
	"SIGPROF":  syscall.SIGPROF,
	"SIGQUIT":  syscall.SIGQUIT,
	"SIGSEGV":  syscall.SIGSEGV,
	"SIGSTOP":  syscall.SIGSTOP,
	"SIGSYS":   syscall.SIGSYS,
	"SIGTERM":  syscall.SIGTERM,
	"SIGTRAP":  syscall.SIGTRAP,
	"SIGTSTP":  syscall.SIGTSTP,
	"SIGTTIN":  syscall.SIGTTIN,
	"SIGTTOU":  syscall.SIGTTOU,
	"SIGURG":   syscall.SIGURG,
	"SIGUSR1":  syscall.SIGUSR1,
	"SIGUSR2":  syscall.SIGUSR2,
	"SIGWINCH": syscall.SIGWINCH,
	"SIGXCPU":  syscall.SIGXCPU,
	"SIGXFSZ":  syscall.SIGXFSZ,
}

type MachineProps struct {
	Name               string
	TimestampMonotonic uint64
	Timestamp          uint64
	NetworkInterfaces  []int32
	ID                 []uint8
	Class              string
	Leader             uint32
	RootDirectory      string
	Service            string
	State              string
	Unit               string
}

type MachineAddrs struct {
	IPv4 net.IP
	//TODO: add parsing for IPv6
	// IPv6         net.IP
}

type MachineConfig struct {
	Bind             hclutils.MapStrStr `codec:"bind"`
	BindReadOnly     hclutils.MapStrStr `codec:"bind_read_only"`
	Boot             bool               `codec:"boot"`
	Capability       []string           `codec:"capability"`
	Command          []string           `codec:"command"`
	Console          string             `codec:"console"`
	Environment      hclutils.MapStrStr `codec:"environment"`
	Ephemeral        bool               `codec:"ephemeral"`
	Image            string             `codec:"image"`
	ImageDownload    *ImageDownloadOpts `codec:"image_download,omitempty"`
	Machine          string             `codec:"machine"`
	NetworkNamespace string             `codec:"network_namespace"`
	NetworkVeth      bool               `codec:"network_veth"`
	NetworkZone      string             `codec:"network_zone"`
	PivotRoot        string             `codec:"pivot_root"`
	Port             hclutils.MapStrStr `codec:"port"`
	Ports            []string           `codec:"ports"` // :-(
	// Deprecated: Nomad dropped support for task network resources in 0.12
	PortMap          hclutils.MapStrInt `codec:"port_map"`
	ProcessTwo       bool               `codec:"process_two"`
	Properties       hclutils.MapStrStr `codec:"properties"`
	ReadOnly         bool               `codec:"read_only"`
	ResolvConf       string             `codec:"resolv_conf"`
	User             string             `codec:"user"`
	UserNamespacing  bool               `codec:"user_namespacing"`
	Volatile         string             `codec:"volatile"`
	WorkingDirectory string             `codec:"working_directory"`
	imagePath        string             `codec:"-"`
	Directory        string             `codec:"directory"`
	LinkJournal      string             `codec:"link_journal"`
	NixOS            string             `codec:"nixos"`
	NixPackages      []string           `codec:"packages"`
	SanitizeNames    *bool              `codec:"sanitize_names"`
}

func (c *MachineConfig) isNixOS() bool       { return c.NixOS != "" }
func (c *MachineConfig) isNixPackages() bool { return len(c.NixPackages) > 0 }

type ImageType string

type ImageProps struct {
	CreationTimestamp     uint64
	Limit                 uint64
	LimitExclusive        uint64
	ModificationTimestamp uint64
	Name                  string
	Path                  string
	ReadOnly              bool
	Type                  string
	Usage                 uint64
	UsageExclusive        uint64
}

type ImageDownloadOpts struct {
	URL    string `codec:"url"`
	Type   string `codec:"type"`
	Force  bool   `codec:"force"`
	Verify string `codec:"verify"`
}

func (c *MachineConfig) ConfigArray() ([]string, error) {
	args := []string{}

	if c.Image != "" {
		// check if image exists
		imageStat, err := os.Stat(c.imagePath)
		if err != nil {
			return nil, err
		}
		imageType := "-i"
		if imageStat.IsDir() {
			imageType = "-D"
		}
		args = append(args, imageType, c.imagePath)
	}

	if c.LinkJournal != "" {
		args = append(args, "--link-journal", c.LinkJournal)
	}
	if c.Directory != "" {
		args = append(args, "--directory", c.Directory)
	}
	if c.Boot {
		args = append(args, "--boot")
	}
	if c.Ephemeral {
		args = append(args, "--ephemeral")
	}
	if c.NetworkVeth {
		args = append(args, "--network-veth")
	}
	if c.NetworkNamespace != "" {
		args = append(args, "--network-namespace-path", c.NetworkNamespace)
	}
	if c.ProcessTwo {
		args = append(args, "--as-pid2")
	}
	if c.ReadOnly {
		args = append(args, "--read-only")
	}
	if c.UserNamespacing {
		args = append(args, "-U")
	}
	if c.Console != "" {
		args = append(args, fmt.Sprintf("--console=%s", c.Console))
	}
	if c.Machine != "" {
		args = append(args, "--machine", c.Machine)
	}
	if c.PivotRoot != "" {
		args = append(args, "--pivot-root", c.PivotRoot)
	}
	if c.ResolvConf != "" {
		args = append(args, "--resolv-conf", c.ResolvConf)
	}
	if c.User != "" {
		args = append(args, "--user", c.User)
	}
	if c.Volatile != "" {
		args = append(args, fmt.Sprintf("--volatile=%s", c.Volatile))
	}
	if c.WorkingDirectory != "" {
		args = append(args, "--chdir", c.WorkingDirectory)
	}
	for k, v := range c.Bind {
		args = append(args, "--bind", k+":"+v)
	}
	for k, v := range c.BindReadOnly {
		args = append(args, "--bind-ro", k+":"+v)
	}
	for k, v := range c.Environment {
		args = append(args, "-E", k+"="+v)
	}
	for _, v := range c.Port {
		args = append(args, "-p", v)
	}
	for k, v := range c.Properties {
		args = append(args, "--property", k+"="+v)
	}
	if len(c.Capability) > 0 {
		args = append(args, "--capability", strings.Join(c.Capability, ","))
	}
	if len(c.NetworkZone) > 0 {
		args = append(args, fmt.Sprintf("--network-zone=%s", c.NetworkZone))
	}
	if len(c.Command) > 0 {
		args = append(args, c.Command...)
	}
	return args, nil
}

func (c *MachineConfig) Validate() error {
	switch c.LinkJournal {
	case "", "no", "host", "try-host", "guest", "try-guest", "auto":
	default:
		return fmt.Errorf("invalid parameter for link_journal")
	}

	switch c.Volatile {
	case "", "yes", "state", "overlay", "no":
	default:
		return fmt.Errorf("invalid parameter for volatile")
	}

	switch c.Console {
	case "", "interactive", "read-only", "passive", "pipe":
	default:
		return fmt.Errorf("invalid parameter for console")
	}

	switch c.ResolvConf {
	case "", "off", "copy-host", "copy-static", "copy-uplink", "copy-stub",
		"replace-host", "replace-static", "replace-uplink", "replace-stub",
		"bind-host", "bind-static", "bind-uplink", "bind-stub", "delete", "auto":
	default:
		return fmt.Errorf("invalid parameter for resolv_conf")
	}

	if c.Boot && c.ProcessTwo {
		return fmt.Errorf("boot and process_two may not be combined")
	}

	if c.Volatile != "" && c.UserNamespacing {
		return fmt.Errorf("volatile and user_namespacing may not be combined")
	}

	if c.ReadOnly && c.UserNamespacing {
		return fmt.Errorf("read_only and user_namespacing may not be combined")
	}

	if c.WorkingDirectory != "" && !filepath.IsAbs(c.WorkingDirectory) {
		return fmt.Errorf("working_directory is not an absolute path")
	}

	if c.PivotRoot != "" {
		for _, p := range strings.Split(c.PivotRoot, ":") {
			if !filepath.IsAbs(p) {
				return fmt.Errorf("pivot_root is not an absolute path")
			}
		}
	}

	if c.Image == "/" && !(c.Ephemeral || c.Volatile == "yes" || c.Volatile == "state") {
		return fmt.Errorf("starting a container from the root directory is not supported. Use ephemeral or volatile")
	}

	if c.ImageDownload != nil {
		switch c.ImageDownload.Type {
		case "raw", "tar":
		default:
			return fmt.Errorf("invalid parameter for image_download.type")
		}

		switch c.ImageDownload.Verify {
		case "no", "checksum", "signature":
		default:
			return fmt.Errorf("invalid parameter for image_download.verify")
		}
	}

	if c.isNixOS() && c.isNixPackages() {
		return fmt.Errorf("nixos and packages may not be combined")
	}

	return nil
}

func (c *MachineConfig) prepareNixOS(dir string) error {
	closure, toplevel, err := nixBuildNixOS(c.NixOS)
	if err != nil {
		return fmt.Errorf("Build of the flake failed: %v", err)
	}

	if c.BindReadOnly == nil {
		c.BindReadOnly = make(hclutils.MapStrStr)
	}

	c.BindReadOnly[toplevel] = toplevel
	c.BindReadOnly[filepath.Join(closure, "registration")] = "/registration"
	c.BindReadOnly[filepath.Join(toplevel, "init")] = "/init"
	c.BindReadOnly[filepath.Join(toplevel, "sw")] = "/sw"

	requisites, err := nixRequisites(closure)
	if err != nil {
		return fmt.Errorf("Couldn't determine flake requisites: %v", err)
	}

	for _, requisite := range requisites {
		c.BindReadOnly[requisite] = requisite
	}

	c.Directory = dir
	c.createUsr()

	if len(c.Command) == 0 {
		c.Command = []string{"/init"}
	}

	return nil
}

func (c *MachineConfig) prepareNixPackages(dir string) error {
	profileLink := filepath.Join(dir, "current-profile")
	profile, err := nixBuildProfile(c.NixPackages, profileLink)
	if err != nil {
		return fmt.Errorf("Build of the flakes failed: %v", err)
	}

	closureLink := filepath.Join(dir, "current-closure")
	closure, err := nixBuildClosure(c.NixPackages, closureLink)
	if err != nil {
		return fmt.Errorf("Build of the flakes failed: %v", err)
	}

	if c.BindReadOnly == nil {
		c.BindReadOnly = make(hclutils.MapStrStr)
	}

	c.BindReadOnly[profile] = profile

	if entries, err := os.ReadDir(profile); err != nil {
		return fmt.Errorf("Couldn't read profile directory: %w", err)
	} else {
		for _, entry := range entries {
			if name := entry.Name(); name != "etc" {
				c.BindReadOnly[filepath.Join(profile, name)] = "/" + name
				continue
			}

			etcEntries, err := os.ReadDir(filepath.Join(profile, "etc"))
			if err != nil {
				return fmt.Errorf("Couldn't read profile's /etc directory: %w", err)
			}

			for _, etcEntry := range etcEntries {
				etcName := etcEntry.Name()
				if etcName == "resolv.conf" {
					// avoid interfering with the --resolv-conf flag
					continue
				}
				c.BindReadOnly[filepath.Join(profile, "etc", etcName)] = "/etc/" + etcName
			}
		}
	}

	c.BindReadOnly[filepath.Join(closure, "registration")] = "/registration"

	requisites, err := nixRequisites(closure)
	if err != nil {
		return fmt.Errorf("Couldn't determine flake requisites: %v", err)
	}

	for _, requisite := range requisites {
		c.BindReadOnly[requisite] = requisite
	}

	c.Directory = dir
	c.createUsr()

	if _, found := c.Environment["PATH"]; !found {
		c.Environment["PATH"] = "/bin"
	}

	return nil
}

func (c *MachineConfig) createUsr() {
	needUsr := true
	for _, guestDir := range c.BindReadOnly {
		if strings.HasPrefix("/usr/", guestDir) {
			needUsr = false
			break
		}
	}

	if needUsr {
		os.MkdirAll(filepath.Join(c.Directory, "usr"), 0777)
	}
}

var machineConn *machine1.Conn
var machineConnM = sync.Mutex{}

func DescribeMachine(name string, timeout time.Duration) (*MachineProps, error) {
	machineConnM.Lock()
	defer machineConnM.Unlock()

	if machineConn == nil {
		var err error
		machineConn, err = machine1.New()
		if err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out while getting machine properties")
		default:
			if p, err := machineConn.DescribeMachine(name); err == nil {
				return &MachineProps{
					Name:               p["Name"].(string),
					TimestampMonotonic: p["TimestampMonotonic"].(uint64),
					Timestamp:          p["Timestamp"].(uint64),
					NetworkInterfaces:  p["NetworkInterfaces"].([]int32),
					ID:                 p["Id"].([]uint8),
					Class:              p["Class"].(string),
					Leader:             p["Leader"].(uint32),
					RootDirectory:      p["RootDirectory"].(string),
					Service:            p["Service"].(string),
					State:              p["State"].(string),
					Unit:               p["Unit"].(string),
				}, nil
			} else {
				time.Sleep(10 * time.Millisecond)
			}
		}
	}
}

func ConfigureIPTablesRules(delete bool, interfaces []string) error {
	if len(interfaces) == 0 {
		return fmt.Errorf("no network interfaces configured")
	}

	table, err := iptables.New()
	if err != nil {
		return err
	}

	for _, i := range interfaces {
		rules := [][]string{
			{"-o", i, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
			{"-i", i, "!", "-o", i, "-j", "ACCEPT"},
			{"-i", i, "-o", i, "-j", "ACCEPT"},
		}

		for _, r := range rules {
			switch ok, err := table.Exists("filter", "FORWARD", r...); {
			case err == nil && !ok:
				err := table.Append("filter", "FORWARD", r...)
				if err != nil {
					return err
				}
			case err == nil && ok && delete:
				err := table.Delete("filter", "FORWARD", r...)
				if err != nil {
					return err
				}
			case err != nil:
				return err
			}
		}
	}

	return nil
}

func (p *MachineProps) GetNetworkInterfaces() ([]string, error) {
	if len(p.NetworkInterfaces) == 0 {
		return nil, fmt.Errorf("machine has no network interfaces assigned")
	}

	n := []string{}
	for _, i := range p.NetworkInterfaces {
		iFace, err := net.InterfaceByIndex(int(i))
		if err != nil {
			return []string{}, err
		}
		n = append(n, iFace.Name)
	}
	return n, nil
}

var dbusConn *dbus.Conn
var dbusConnM = sync.Mutex{}

func MachineAddresses(name string, timeout time.Duration) (*MachineAddrs, error) {
	dbusConnM.Lock()
	defer dbusConnM.Unlock()

	if dbusConn == nil {
		var err error
		dbusConn, err = setupPrivateSystemBus()
		if err != nil {
			return nil, fmt.Errorf("failed to connect to dbus: %+v", err)
		}
	}

	obj := dbusConn.Object("org.freedesktop.machine1", dbus.ObjectPath(dbusPath))

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var result *dbus.Call
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out while getting machine addresses: %+v", result.Err)
		default:
			result = obj.Call(fmt.Sprintf("%s.%s", dbusInterface, "GetMachineAddresses"), 0, name)
			if result.Err != nil {
				return nil, fmt.Errorf("failed to call dbus: %+v", result.Err)
			}

			addrs := MachineAddrs{}

			for _, v := range result.Body[0].([][]interface{}) {
				t := v[0].(int32)
				a := v[1].([]uint8)
				if t == 2 {
					ip := net.IP{}
					for _, o := range a {
						ip = append(ip, byte(o))
					}
					if !ip.IsLinkLocalUnicast() {
						addrs.IPv4 = ip
					}
				}
			}

			if len(addrs.IPv4) > 0 {
				return &addrs, nil
			}

			time.Sleep(10 * time.Millisecond)
		}
	}
}

func isInstalled() error {
	_, err := exec.LookPath("systemd-nspawn")
	if err != nil {
		return err
	}
	_, err = exec.LookPath("machinectl")
	if err != nil {
		return err
	}
	return nil
}

// systemdVersion uses dbus to check which version of systemd is installed.
func systemdVersion() (string, error) {
	// check if systemd is running
	if !systemdUtil.IsRunningSystemd() {
		return "null", fmt.Errorf("systemd is not running")
	}
	bus, err := systemdDbus.NewSystemdConnection()
	if err != nil {
		return "null", err
	}
	defer bus.Close()
	// get the systemd version
	verString, err := bus.GetManagerProperty("Version")
	if err != nil {
		return "null", err
	}
	// lose the surrounding quotes
	verNumString, err := strconv.Unquote(verString)
	if err != nil {
		return "null", err
	}
	// trim possible version suffix like in "242.19-1"
	verNum := strings.Split(verNumString, ".")[0]
	return verNum, nil
}

func setupPrivateSystemBus() (conn *dbus.Conn, err error) {
	conn, err = dbus.SystemBusPrivate()
	if err != nil {
		return nil, err
	}
	methods := []dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}
	if err = conn.Auth(methods); err != nil {
		conn.Close()
		conn = nil
		return
	}
	if err = conn.Hello(); err != nil {
		conn.Close()
		conn = nil
	}
	return conn, nil
}

func DescribeImage(name string) (*ImageProps, error) {
	dbusConnM.Lock()
	defer dbusConnM.Unlock()

	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, err
	}

	img := conn.Object("org.freedesktop.machine1", "/org/freedesktop/machine1")
	var path dbus.ObjectPath

	err = img.Call("org.freedesktop.machine1.Manager.GetImage", 0, name).Store(&path)
	if err != nil {
		return nil, err
	}

	obj := conn.Object("org.freedesktop.machine1", path)
	props := make(map[string]interface{})

	err = obj.Call("org.freedesktop.DBus.Properties.GetAll", 0, "").Store(&props)
	if err != nil {
		return nil, err
	}

	return &ImageProps{
		CreationTimestamp:     props["CreationTimestamp"].(uint64),
		Limit:                 props["Limit"].(uint64),
		LimitExclusive:        props["LimitExclusive"].(uint64),
		ModificationTimestamp: props["ModificationTimestamp"].(uint64),
		Name:                  props["Name"].(string),
		Path:                  props["Path"].(string),
		ReadOnly:              props["ReadOnly"].(bool),
		Type:                  props["Type"].(string),
		Usage:                 props["Usage"].(uint64),
		UsageExclusive:        props["UsageExclusive"].(uint64),
	}, nil
}

func nixBuildProfile(flakes []string, link string) (string, error) {
	cmd := exec.Command("nix", append([]string{"profile", "install", "--no-write-lock-file", "--profile", link}, flakes...)...)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%v failed: %s. Err: %v", cmd.Args, stderr.String(), err)
	}

	if target, err := os.Readlink(link); err == nil {
		return os.Readlink(filepath.Join(filepath.Dir(link), target))
	} else {
		return "", err
	}
}

func nixBuildClosure(flakes []string, link string) (string, error) {
	for i, flake := range flakes {
		r := regexp.MustCompile(`^path:\.(.*)$`)
		dir, err := os.Getwd()
		if err != nil {
			return "", err
		}
		flakes[i] = r.ReplaceAllString(flake, `path:`+dir+`$1`)
	}

	j, err := json.Marshal(flakes)
	if err != nil {
		return "", err
	}

	cmd := exec.Command(
		"nix", "build",
		"--out-link", link,
		"--expr", closureNix,
		"--impure",
		"--no-write-lock-file",
		"--argstr", "flakes", string(j))

	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%v failed: %s. Err: %v", cmd.Args, stderr.String(), err)
	}

	return os.Readlink(link)
}

func nixBuildNixOS(flakePrefix string) (string, string, error) {
	nixos := fmt.Sprintf("%s.config.system.build", flakePrefix)
	closurePath, err := nixBuild(nixos + ".closure")
	if err != nil {
		return "", "", fmt.Errorf("buildClosure failed: %v", err)
	}

	toplevelPath, err := nixBuild(nixos + ".toplevel")
	if err != nil {
		return "", "", fmt.Errorf("buildToplevel failed: %v", err)
	}

	return closurePath, toplevelPath, nil
}

type nixBuildResult struct {
	DrvPath string
	Outputs map[string]string
}

func nixBuild(flake string) (string, error) {
	cmd := exec.Command("nix", "build", "--no-link", "--no-write-lock-file", "--json", flake)

	stdout := &bytes.Buffer{}
	cmd.Stdout = stdout

	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%v failed: %s. Err: %v", cmd.Args, stderr.String(), err)
	}

	result := []*nixBuildResult{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return "", err
	}

	return result[0].Outputs["out"], nil
}

type nixPathInfo struct {
	Path             string   `json:"path"`
	NarHash          string   `json:"narHash"`
	NarSize          uint64   `json:"narSize"`
	References       []string `json:"references"`
	Deriver          string   `json:"deriver"`
	RegistrationTime uint64   `json:"registrationTime"`
	Signatures       []string `json:"signatures"`
}

func nixRequisites(path string) ([]string, error) {
	cmd := exec.Command("nix", "path-info", "--json", "--recursive", path)

	stdout := &bytes.Buffer{}
	cmd.Stdout = stdout

	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%v failed: %s. Err: %v", cmd.Args, stderr.String(), err)
	}

	result := []*nixPathInfo{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, err
	}

	requisites := []string{}
	for _, result := range result {
		requisites = append(requisites, result.Path)
	}

	return requisites, nil
}

func DownloadImage(url, name, verify, imageType string, force bool, logger hclog.Logger) error {
	c, err := import1.New()
	if err != nil {
		return err
	}

	if imageType != TarImage && imageType != RawImage {
		return fmt.Errorf("unsupported image type")
	}

	// systemd-importd only allows one transfer for each unique URL at a
	// time. To not run into API errors, we need to ensure we do not try to
	// download an image from the same URL multiple times at one. We do this
	// by creating a simple map containing a Mutex for each URL and only
	// start our download if we can hold the lock for a given URL. This
	// naively assumes we are the only process making regular use of the
	// systemd-importd api on the host.
	//
	// In the future it would probably be better to make use of the built-in
	// signals in systemd-importd as described here:
	// https://www.freedesktop.org/wiki/Software/systemd/importd/

	// get global lock
	logger.Debug("waiting on global download lock")
	transferMut.Lock()
	// get lock for given remote
	l, ok := mutMap[url]
	if !ok {
		// create it if it does not exist
		var m sync.Mutex
		l = &m
		mutMap[url] = &m
	} else {
		logger.Debug("remote lock exists", "remote", url)
	}
	// release global lock
	transferMut.Unlock()
	// get lock for remote
	logger.Debug("waiting on remote lock", "remote", url)
	l.Lock()
	// release lock for remote when done
	defer l.Unlock()

	var t *import1.Transfer
	switch imageType {
	case TarImage:
		t, err = c.PullTar(url, name, verify, force)
	case RawImage:
		t, err = c.PullRaw(url, name, verify, force)
	default:
		return fmt.Errorf("unsupported image type")
	}
	if err != nil {
		return err
	}

	// wait until transfer is finished
	logger.Info("downloading image", "image", name)
	done := false
	ticker := time.NewTicker(2 * time.Second)
	for !done {
		select {
		case <-ticker.C:
			tf, _ := c.ListTransfers()
			if len(tf) == 0 {
				done = true
				ticker.Stop()
				continue
			}
			found := false
			for _, v := range tf {
				if v.Id == t.Id {
					found = true
					if !(math.IsNaN(v.Progress) || math.IsInf(v.Progress, 0) || math.Abs(v.Progress) == math.MaxFloat64) {
						logger.Info("downloading image", "image", name, "progress", v.Progress)
					}
				}
			}
			if !found {
				done = true
				ticker.Stop()
			}
		}
	}

	logger.Info("downloaded image", "image", name)
	return nil
}

func (c *MachineConfig) GetImagePath() (string, error) {
	// check if image is absolute or relative path
	imagePath := c.Image
	if !filepath.IsAbs(c.Image) {
		pwd, e := os.Getwd()
		if e != nil {
			return "", e
		}
		imagePath = filepath.Join(pwd, c.Image)
	}
	// check if image exists
	_, err := os.Stat(imagePath)
	if err == nil {
		return imagePath, err
	}
	// check if image is known to machinectl
	p, err := DescribeImage(c.Image)
	if err != nil {
		return "", err
	}
	return p.Path, nil
}

func readEnviron(pid uint32) map[string]string {
	environ, err := os.Open(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		panic(err)
	}
	defer environ.Close()

	s := bufio.NewScanner(environ)
	s.Split(
		func(data []byte, atEOF bool) (advance int, token []byte, err error) {
			if atEOF && len(data) == 0 {
				return 0, nil, nil
			}

			if i := bytes.IndexByte(data, '\000'); i >= 0 {
				return i + 1, data[0:i], nil
			}

			if atEOF {
				return len(data), data, nil
			}

			return 0, nil, nil
		})

	env := map[string]string{}

	for s.Scan() {
		foo := strings.SplitN(s.Text(), "=", 2)
		env[foo[0]] = foo[1]
	}

	return env
}
