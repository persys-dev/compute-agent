package runtime

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/digitalocean/go-libvirt"
	"github.com/persys/compute-agent/pkg/models"
	"github.com/sirupsen/logrus"
)

// VMRuntime manages KVM virtual machine workloads via libvirt
type VMRuntime struct {
	conn   *libvirt.Libvirt
	logger *logrus.Entry
}

func NewVMRuntime(uri string, logger *logrus.Logger) (*VMRuntime, error) {
	// For simplicity, this connects to the local libvirt UNIX socket.
	// In production, you'd want to handle remote connections properly and respect the uri parameter.
	socketPath := "/var/run/libvirt/libvirt-sock"
	netConn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to dial libvirt socket %s: %w", socketPath, err)
	}

	// Create a custom dialer that uses our pre-established connection
	dialer := &unixDialer{conn: netConn}
	conn := libvirt.NewWithDialer(dialer)

	// Perform libvirt handshake/initialization
	if err := conn.Connect(); err != nil {
		netConn.Close()
		return nil, fmt.Errorf("failed to handshake with libvirt: %w", err)
	}

	return &VMRuntime{
		conn:   conn,
		logger: logger.WithField("runtime", "vm"),
	}, nil
}

// unixDialer implements socket.Dialer for libvirt connections
type unixDialer struct {
	conn net.Conn
}

func (ud *unixDialer) Dial() (net.Conn, error) {
	if ud.conn == nil {
		return nil, fmt.Errorf("connection is nil")
	}
	return ud.conn, nil
}

func (v *VMRuntime) Type() models.WorkloadType {
	return models.WorkloadTypeVM
}

func (v *VMRuntime) Create(ctx context.Context, workload *models.Workload) error {
	spec, err := v.parseSpec(workload.Spec)
	if err != nil {
		return fmt.Errorf("failed to parse VM spec: %w", err)
	}

	// Create cloud-init ISO if needed
	if spec.CloudInit != "" || spec.CloudInitConfig != nil {
		isoPath, err := v.createCloudInitISO(workload.ID, spec)
		if err != nil {
			return fmt.Errorf("failed to create cloud-init ISO: %w", err)
		}
		if isoPath != "" {
			// Add ISO to disks - mark as bootable CD-ROM
			spec.Disks = append(spec.Disks, models.DiskConfig{
				Path:   isoPath,
				Device: "hdb",
				Format: "raw",   // ISO files use raw format
				Type:   "cdrom", // String value
				Boot:   true,    // Mark as bootable
				SizeGB: 0,
			})
		}
	}

	// Create disk files first
	for _, diskCfg := range spec.Disks {
		if diskCfg.Type != models.DiskTypeCDROM {
			if err := v.createDisk(&diskCfg); err != nil {
				return fmt.Errorf("failed to create disk %s: %w", diskCfg.Path, err)
			}
		}
	}

	// Generate libvirt XML
	domainXML, err := v.generateDomainXML(workload.ID, spec)
	if err != nil {
		return fmt.Errorf("failed to generate domain XML: %w", err)
	}

	// Define the domain
	domain, err := v.conn.DomainDefineXML(domainXML)
	if err != nil {
		return fmt.Errorf("failed to define domain: %w", err)
	}

	v.logger.Infof("Created VM domain: %s (UUID: %s)", workload.ID, domain.UUID)
	return nil
}

func (v *VMRuntime) Start(ctx context.Context, id string) error {
	domain, err := v.conn.DomainLookupByName(id)
	if err != nil {
		return fmt.Errorf("failed to lookup domain: %w", err)
	}

	if err := v.conn.DomainCreate(domain); err != nil {
		return fmt.Errorf("failed to start domain: %w", err)
	}

	v.logger.Infof("Started VM: %s", id)
	return nil
}

func (v *VMRuntime) Stop(ctx context.Context, id string) error {
	domain, err := v.conn.DomainLookupByName(id)
	if err != nil {
		return fmt.Errorf("failed to lookup domain: %w", err)
	}

	// Graceful shutdown
	if err := v.conn.DomainShutdown(domain); err != nil {
		return fmt.Errorf("failed to shutdown domain: %w", err)
	}

	v.logger.Infof("Stopped VM: %s", id)
	return nil
}

func (v *VMRuntime) Delete(ctx context.Context, id string) error {
	domain, err := v.conn.DomainLookupByName(id)
	if err != nil {
		// Already deleted
		return nil
	}

	// Destroy if running
	state, _, err := v.conn.DomainGetState(domain, 0)
	if err == nil && state == int32(libvirt.DomainRunning) {
		_ = v.conn.DomainDestroy(domain)
	}

	// Undefine the domain
	if err := v.conn.DomainUndefine(domain); err != nil {
		return fmt.Errorf("failed to undefine domain: %w", err)
	}

	v.logger.Infof("Deleted VM: %s", id)
	return nil
}

func (v *VMRuntime) Status(ctx context.Context, id string) (models.ActualState, string, error) {
	domain, err := v.conn.DomainLookupByName(id)
	if err != nil {
		return models.ActualStateUnknown, "domain not found", nil
	}

	state, reason, err := v.conn.DomainGetState(domain, 0)
	if err != nil {
		return models.ActualStateUnknown, "", fmt.Errorf("failed to get domain state: %w", err)
	}

	var actualState models.ActualState
	var message string

	switch state {
	case int32(libvirt.DomainRunning):
		actualState = models.ActualStateRunning
		message = "running"
	case int32(libvirt.DomainPaused):
		actualState = models.ActualStateStopped
		message = "paused"
	case int32(libvirt.DomainShutdown), int32(libvirt.DomainShutoff):
		actualState = models.ActualStateStopped
		message = "shutdown"
	case int32(libvirt.DomainCrashed):
		actualState = models.ActualStateFailed
		message = "crashed"
	case int32(libvirt.DomainBlocked), int32(libvirt.DomainNostate):
		actualState = models.ActualStatePending
		message = fmt.Sprintf("state: %d, reason: %d", state, reason)
	default:
		actualState = models.ActualStateUnknown
		message = fmt.Sprintf("unknown state: %d", state)
	}

	return actualState, message, nil
}

func (v *VMRuntime) List(ctx context.Context) ([]string, error) {
	domains, _, err := v.conn.ConnectListAllDomains(1, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to list domains: %w", err)
	}

	var names []string
	for _, domain := range domains {
		names = append(names, domain.Name)
	}

	return names, nil
}

func (v *VMRuntime) Healthy(ctx context.Context) error {
	// Check connection by attempting to list domains
	_, _, err := v.conn.ConnectListAllDomains(1, 0)
	if err != nil {
		return fmt.Errorf("failed to check libvirt connection: %w", err)
	}
	return nil
}

// Helper functions

func (v *VMRuntime) parseSpec(specMap map[string]interface{}) (*models.VMSpec, error) {
	data, err := json.Marshal(specMap)
	if err != nil {
		return nil, err
	}

	var spec models.VMSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, err
	}

	return &spec, nil
}

// createDisk creates a QCOW2 disk image for the VM
func (v *VMRuntime) createDisk(diskCfg *models.DiskConfig) error {
	// Ensure the directory exists
	diskDir := filepath.Dir(diskCfg.Path)
	if err := os.MkdirAll(diskDir, 0755); err != nil {
		return fmt.Errorf("failed to create disk directory %s: %w", diskDir, err)
	}

	// Skip if disk already exists
	if _, err := os.Stat(diskCfg.Path); err == nil {
		v.logger.Warnf("Disk already exists at %s, skipping creation", diskCfg.Path)
		return nil
	}

	// Create QCOW2 disk using qemu-img
	// Format: qemu-img create -f qcow2 /path/to/disk.qcow2 20G
	cmd := exec.Command(
		"qemu-img",
		"create",
		"-f", diskCfg.Format,
		diskCfg.Path,
		fmt.Sprintf("%dG", diskCfg.SizeGB),
	)

	// Run the command - this will succeed or fail based on permissions
	output, err := cmd.CombinedOutput()
	if err != nil {
		v.logger.Errorf("qemu-img output: %s", string(output))
		return fmt.Errorf("failed to create disk image: %w", err)
	}

	v.logger.Infof("Created QCOW2 disk: %s (%dGB)", diskCfg.Path, diskCfg.SizeGB)
	return nil
}

// createCloudInitISO creates a cloud-init ISO for VM configuration
func (v *VMRuntime) createCloudInitISO(vmID string, spec *models.VMSpec) (string, error) {
	// Only create if cloud-init is specified
	if spec.CloudInit == "" && spec.CloudInitConfig == nil {
		return "", nil
	}

	// Create temporary directory for cloud-init files
	tmpDir, err := os.MkdirTemp("", fmt.Sprintf("cloud-init-%s-", vmID))
	if err != nil {
		return "", fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create meta-data
	metaData := fmt.Sprintf(`{
  "instance-id": "%s",
  "hostname": "%s",
  "local-ipv4": "127.0.0.1"
}`, vmID, spec.Name)

	metaDataPath := filepath.Join(tmpDir, "meta-data")
	if err := os.WriteFile(metaDataPath, []byte(metaData), 0644); err != nil {
		return "", fmt.Errorf("failed to write meta-data: %w", err)
	}

	// Create user-data
	var userData string
	if spec.CloudInitConfig != nil && spec.CloudInitConfig.UserData != "" {
		userData = spec.CloudInitConfig.UserData
	} else if spec.CloudInit != "" {
		userData = spec.CloudInit
	} else {
		userData = "#!/bin/bash\necho 'Cloud-init configured'\n"
	}

	userDataPath := filepath.Join(tmpDir, "user-data")
	if err := os.WriteFile(userDataPath, []byte(userData), 0644); err != nil {
		return "", fmt.Errorf("failed to write user-data: %w", err)
	}

	// Generate ISO filename in a standard location
	isoDir := "/var/lib/libvirt/images"
	if err := os.MkdirAll(isoDir, 0755); err != nil {
		v.logger.Warnf("Failed to create ISO directory %s, using /tmp", isoDir)
		isoDir = "/tmp"
	}
	isoPath := filepath.Join(isoDir, fmt.Sprintf("%s-cloud-init.iso", vmID))

	// Create ISO using mkisofs or genisoimage
	cmd := exec.Command(
		"mkisofs",
		"-output", isoPath,
		"-volid", "cidata",
		"-joliet",
		"-rock",
		"-file-mode", "0644",
		"-dir-mode", "0755",
		userDataPath,
		metaDataPath,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		v.logger.Errorf("mkisofs output: %s", string(output))
		return "", fmt.Errorf("failed to create cloud-init ISO: %w", err)
	}

	v.logger.Infof("Created cloud-init ISO: %s", isoPath)
	return isoPath, nil
}

// Helper functions
type Domain struct {
	XMLName    xml.Name `xml:"domain"`
	Type       string   `xml:"type,attr"`
	Name       string   `xml:"name"`
	Memory     Memory   `xml:"memory"`
	CurrentMem Memory   `xml:"currentMemory"`
	VCPU       VCPU     `xml:"vcpu"`
	OS         OS       `xml:"os"`
	Features   Features `xml:"features"`
	CPU        CPU      `xml:"cpu"`
	Clock      Clock    `xml:"clock"`
	Devices    Devices  `xml:"devices"`
}

type Memory struct {
	Unit  string `xml:"unit,attr"`
	Value int64  `xml:",chardata"`
}

type VCPU struct {
	Placement string `xml:"placement,attr"`
	Value     int    `xml:",chardata"`
}

type OS struct {
	Type OSType `xml:"type"`
}

type OSType struct {
	Arch    string `xml:"arch,attr"`
	Machine string `xml:"machine,attr"`
	Value   string `xml:",chardata"`
}

type Boot struct {
	Dev   string `xml:"dev,attr,omitempty"`   // OS boot device (hd, cdrom, etc.)
	Order string `xml:"order,attr,omitempty"` // Disk boot order (1, 2, etc.)
}

type Features struct {
	ACPI struct{} `xml:"acpi"`
	APIC struct{} `xml:"apic"`
}

type CPU struct {
	Mode string `xml:"mode,attr"`
}

type Clock struct {
	Offset string `xml:"offset,attr"`
}

type Devices struct {
	Emulator   string      `xml:"emulator"`
	Disks      []Disk      `xml:"disk"`
	Interfaces []Interface `xml:"interface"`
	Serial     Serial      `xml:"serial"`
	Console    Console     `xml:"console"`
	Graphics   Graphics    `xml:"graphics"`
}

type Disk struct {
	Type   string     `xml:"type,attr"`
	Device string     `xml:"device,attr"`
	Driver DiskDriver `xml:"driver"`
	Source DiskSource `xml:"source"`
	Target DiskTarget `xml:"target"`
	Boot   *Boot      `xml:"boot,omitempty"` // Boot order
}

type DiskDriver struct {
	Name string `xml:"name,attr"`
	Type string `xml:"type,attr"`
}

type DiskSource struct {
	File string `xml:"file,attr"`
}

type DiskTarget struct {
	Dev string `xml:"dev,attr"`
	Bus string `xml:"bus,attr"`
}

type Interface struct {
	Type   string          `xml:"type,attr"`
	MAC    MAC             `xml:"mac"`
	Source InterfaceSource `xml:"source"`
	Model  InterfaceModel  `xml:"model"`
}

type MAC struct {
	Address string `xml:"address,attr"`
}

type InterfaceSource struct {
	Network string `xml:"network,attr,omitempty"`
	Bridge  string `xml:"bridge,attr,omitempty"`
}

type InterfaceModel struct {
	Type string `xml:"type,attr"`
}

type Serial struct {
	Type   string       `xml:"type,attr"`
	Target SerialTarget `xml:"target"`
}

type Console struct {
	Type   string        `xml:"type,attr"`
	Target ConsoleTarget `xml:"target"`
}

type SerialTarget struct {
	Port string `xml:"port,attr"`
}

type ConsoleTarget struct {
	Type string `xml:"type,attr"`
	Port string `xml:"port,attr"`
}

type Graphics struct {
	Type   string `xml:"type,attr"`
	Port   string `xml:"port,attr"`
	Listen string `xml:"listen,attr"`
}

func (v *VMRuntime) generateDomainXML(name string, spec *models.VMSpec) (string, error) {
	domain := Domain{
		Type: "kvm",
		Name: name,
		Memory: Memory{
			Unit:  "MiB",
			Value: spec.MemoryMB,
		},
		CurrentMem: Memory{
			Unit:  "MiB",
			Value: spec.MemoryMB,
		},
		VCPU: VCPU{
			Placement: "static",
			Value:     spec.VCPUs,
		},
		OS: OS{
			Type: OSType{
				Arch:    "x86_64",
				Machine: "pc-q35-5.2",
				Value:   "hvm",
			},
		},
		Features: Features{},
		CPU:      CPU{Mode: "host-passthrough"},
		Clock:    Clock{Offset: "utc"},
		Devices: Devices{
			Emulator: "/usr/bin/qemu-system-x86_64",
			Serial:   Serial{Type: "pty", Target: SerialTarget{Port: "0"}},
			Console:  Console{Type: "pty", Target: ConsoleTarget{Type: "serial", Port: "0"}},
			Graphics: Graphics{Type: "vnc", Port: "-1", Listen: "0.0.0.0"},
		},
	}

	// Add disks and CDROMs
	bootOrder := 1
	for _, diskCfg := range spec.Disks {
		device := "disk"
		bus := "virtio"

		if diskCfg.Type == "cdrom" {
			device = "cdrom"
			bus = "sata"
		}

		disk := Disk{
			Type:   "file",
			Device: device,
			Driver: DiskDriver{Name: "qemu", Type: diskCfg.Format},
			Source: DiskSource{File: diskCfg.Path},
			Target: DiskTarget{Dev: diskCfg.Device, Bus: bus},
		}

		// Set boot order for bootable disks
		if diskCfg.Boot {
			disk.Boot = &Boot{Order: fmt.Sprintf("%d", bootOrder)}
			bootOrder++
		}

		domain.Devices.Disks = append(domain.Devices.Disks, disk)
	}

	// Add network interfaces
	for _, netCfg := range spec.Networks {
		iface := Interface{
			Type: "network",
			MAC:  MAC{Address: netCfg.MACAddress},
			Source: InterfaceSource{
				Network: netCfg.Network,
			},
			Model: InterfaceModel{Type: "virtio"},
		}
		domain.Devices.Interfaces = append(domain.Devices.Interfaces, iface)
	}

	// Marshal to XML
	xmlData, err := xml.MarshalIndent(domain, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal domain XML: %w", err)
	}

	// If cloud-init is configured, we'll inject it via metadata
	// This will be handled by libvirt cloud-init datasource if available
	return xml.Header + string(xmlData), nil
}
