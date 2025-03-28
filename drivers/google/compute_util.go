package google

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/docker/machine/drivers/driverutil"
	"github.com/docker/machine/libmachine/log"
	raw "google.golang.org/api/compute/v1"

	"errors"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/googleapi"
)

// ComputeUtil is used to wrap the raw GCE API code and store common parameters.
type ComputeUtil struct {
	zone              string
	instanceName      string
	userName          string
	project           string
	diskTypeURL       string
	address           string
	networkProject    string
	network           string
	subnetwork        string
	preemptible       bool
	useInternalIP     bool
	useInternalIPOnly bool
	service           *raw.Service
	zoneURL           string
	globalURL         string
	SwarmMaster       bool
	SwarmHost         string
	openPorts         []string
	minCPUPlatform    string
	accelerator       string
	maintenancePolicy string
	skipFirewall      bool

	operationBackoffFactory *backoffFactory
}

const (
	apiURL            = "https://www.googleapis.com/compute/v1/projects/"
	firewallRule      = "docker-machines"
	dockerPort        = "2376"
	firewallTargetTag = "docker-machine"
)

var (
	networkRegex        = regexp.MustCompile(`/networks/`)
	networkProjectRegex = regexp.MustCompile(apiURL + `(?P<project_name>[^/]+)/global/networks/(?P<network_name>[A-Za-z-]+)`)
)

// NewComputeUtil creates and initializes a ComputeUtil.
func newComputeUtil(driver *Driver) (*ComputeUtil, error) {
	client, err := google.DefaultClient(oauth2.NoContext, raw.ComputeScope)
	if err != nil {
		return nil, err
	}

	service, err := raw.New(client)
	if err != nil {
		return nil, err
	}

	// networkProject is equals to the main project set for the driver, but if the network property is a complete api
	// url we will override with the ones specified inside it. This will allow to setup runners in a project with a
	// shared network
	networkProject := driver.Project
	if matches := networkProjectRegex.FindStringSubmatch(driver.Network); len(matches) > 0 {
		networkProject = matches[1]
	}

	return &ComputeUtil{
		zone:                    driver.Zone,
		instanceName:            driver.MachineName,
		userName:                driver.SSHUser,
		project:                 driver.Project,
		diskTypeURL:             driver.DiskType,
		address:                 driver.Address,
		networkProject:          networkProject,
		network:                 driver.Network,
		subnetwork:              driver.Subnetwork,
		preemptible:             driver.Preemptible,
		useInternalIP:           driver.UseInternalIP,
		useInternalIPOnly:       driver.UseInternalIPOnly,
		service:                 service,
		zoneURL:                 apiURL + driver.Project + "/zones/" + driver.Zone,
		globalURL:               apiURL + driver.Project + "/global",
		SwarmMaster:             driver.SwarmMaster,
		SwarmHost:               driver.SwarmHost,
		openPorts:               driver.OpenPorts,
		operationBackoffFactory: driver.OperationBackoffFactory,
		minCPUPlatform:          driver.MinCPUPlatform,
		accelerator:             driver.Accelerator,
		maintenancePolicy:       driver.MaintenancePolicy,
		skipFirewall:            driver.SkipFirewall,
	}, nil
}

func (c *ComputeUtil) acceleratorCountAndType() (int, string) {
	if c.accelerator == "" {
		return 0, ""
	}

	split := strings.Split(strings.TrimSpace(c.accelerator), ",")
	count := 1
	acceleratorType := ""

	for _, kvStr := range split {
		kv := strings.Split(kvStr, "=")

		if len(kv) != 2 {
			log.Infof("Invalid key/value parameter for accelerator: %s, ignoring", kvStr)
			continue
		}

		key, value := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
		switch key {
		case "count":
			var err error
			count, err = strconv.Atoi(value)
			if err != nil {
				log.Infof("Failed to parse %q as count, disabling accelerator", value)
				return 0, ""
			}
		case "type":
			acceleratorType = strings.TrimSpace(value)
		default:
			log.Infof("Invalid accelerator defined %q, should be count=N,type=type", c.accelerator)
			return 0, ""
		}
	}

	return count, acceleratorType
}

func (c *ComputeUtil) diskName() string {
	return c.instanceName + "-disk"
}

func (c *ComputeUtil) diskType() string {
	return apiURL + c.project + "/zones/" + c.zone + "/diskTypes/" + c.diskTypeURL
}

// disk returns the persistent disk attached to the vm.
func (c *ComputeUtil) disk() (*raw.Disk, error) {
	return c.service.Disks.Get(c.project, c.zone, c.diskName()).Do()
}

// deleteDisk deletes the persistent disk.
func (c *ComputeUtil) deleteDisk() error {
	disk, _ := c.disk()
	if disk == nil {
		return nil
	}

	log.Infof("Deleting disk.")
	op, err := c.service.Disks.Delete(c.project, c.zone, c.diskName()).Do()
	if err != nil {
		return err
	}

	log.Infof("Waiting for disk to delete.")
	return c.waitForRegionalOp(op.Name)
}

// staticAddress returns the external static IP address.
func (c *ComputeUtil) staticAddress() (string, error) {
	// is the address a name?
	isName, err := regexp.MatchString("[a-z]([-a-z0-9]*[a-z0-9])?", c.address)
	if err != nil {
		return "", err
	}

	if !isName {
		return c.address, nil
	}

	// resolve the address by name
	externalAddress, err := c.service.Addresses.Get(c.project, c.region(), c.address).Do()
	if err != nil {
		return "", err
	}

	return externalAddress.Address, nil
}

func (c *ComputeUtil) region() string {
	return c.zone[:len(c.zone)-2]
}

func (c *ComputeUtil) firewallRule() (*raw.Firewall, error) {
	log.Infof("Getting firewall rule in project %s", c.networkProject)
	return c.service.Firewalls.Get(c.networkProject, firewallRule).Do()
}

func missingOpenedPorts(rule *raw.Firewall, ports []string) map[string][]string {
	missing := map[string][]string{}
	opened := map[string]bool{}

	for _, allowed := range rule.Allowed {
		for _, allowedPort := range allowed.Ports {
			opened[allowedPort+"/"+allowed.IPProtocol] = true
		}
	}

	for _, p := range ports {
		port, proto := driverutil.SplitPortProto(p)
		if !opened[port+"/"+proto] {
			missing[proto] = append(missing[proto], port)
		}
	}

	return missing
}

func (c *ComputeUtil) portsUsed() ([]string, error) {
	ports := []string{dockerPort + "/tcp"}

	if c.SwarmMaster {
		u, err := url.Parse(c.SwarmHost)
		if err != nil {
			return nil, fmt.Errorf("error authorizing port for swarm: %s", err)
		}

		swarmPort := strings.Split(u.Host, ":")[1]
		ports = append(ports, swarmPort+"/tcp")
	}
	for _, p := range c.openPorts {
		port, proto := driverutil.SplitPortProto(p)
		ports = append(ports, port+"/"+proto)
	}

	return ports, nil
}

// openFirewallPorts configures the firewall to open docker and swarm ports.
func (c *ComputeUtil) openFirewallPorts(d *Driver) error {
	if c.skipFirewall {
		log.Infof("Skipping opening firewall ports")
		return nil
	}

	log.Infof("Opening firewall ports")

	create := false
	rule, err := c.firewallRule()
	if err != nil {
		return fmt.Errorf("requesting firewall rule: %v", err)
	}

	if rule == nil {
		create = true
		net := c.globalURL + "/networks/" + d.Network
		if networkRegex.MatchString(d.Network) {
			net = d.Network
		}

		rule = &raw.Firewall{
			Name:         firewallRule,
			Allowed:      []*raw.FirewallAllowed{},
			SourceRanges: []string{"0.0.0.0/0"},
			TargetTags:   []string{firewallTargetTag},
			Network:      net,
		}
	}

	portsUsed, err := c.portsUsed()
	if err != nil {
		return err
	}

	missingPorts := missingOpenedPorts(rule, portsUsed)
	if len(missingPorts) == 0 {
		return nil
	}
	for proto, ports := range missingPorts {
		rule.Allowed = append(rule.Allowed, &raw.FirewallAllowed{
			IPProtocol: proto,
			Ports:      ports,
		})
	}

	var op *raw.Operation
	if create {
		op, err = c.service.Firewalls.Insert(c.networkProject, rule).Do()
	} else {
		op, err = c.service.Firewalls.Update(c.networkProject, firewallRule, rule).Do()
	}

	if err != nil {
		return err
	}

	return c.waitForGlobalOp(op.Name)
}

// instance retrieves the instance.
func (c *ComputeUtil) instance() (*raw.Instance, error) {
	return c.service.Instances.Get(c.project, c.zone, c.instanceName).Do()
}

// createInstance creates a GCE VM instance.
func (c *ComputeUtil) createInstance(d *Driver) error {
	log.Infof("Creating instance")

	var net string
	if strings.Contains(d.Network, "/networks/") {
		net = d.Network
	} else {
		net = c.globalURL + "/networks/" + d.Network
	}

	metadata, err := prepareMetadata(d)
	if err != nil {
		return err
	}

	instance := &raw.Instance{
		Name:           c.instanceName,
		Description:    "docker host vm",
		MachineType:    c.zoneURL + "/machineTypes/" + d.MachineType,
		MinCpuPlatform: c.minCPUPlatform,
		Disks: []*raw.AttachedDisk{
			{
				Boot:       true,
				AutoDelete: true,
				Type:       "PERSISTENT",
				Mode:       "READ_WRITE",
			},
		},
		NetworkInterfaces: []*raw.NetworkInterface{
			{
				Network: net,
			},
		},
		Tags: &raw.Tags{
			Items: parseTags(d),
		},
		ServiceAccounts: []*raw.ServiceAccount{
			{
				Email:  d.ServiceAccount,
				Scopes: strings.Split(d.Scopes, ","),
			},
		},
		Scheduling: &raw.Scheduling{
			Preemptible: c.preemptible,
		},
		Labels:   parseLabels(d),
		Metadata: metadata,
	}

	if c.maintenancePolicy != "" {
		instance.Scheduling.OnHostMaintenance = c.maintenancePolicy
	}

	acceleratorCount, acceleratorType := c.acceleratorCountAndType()

	if acceleratorCount > 0 && len(acceleratorType) > 0 {
		instance.GuestAccelerators = []*raw.AcceleratorConfig{
			{
				AcceleratorCount: int64(acceleratorCount),
				AcceleratorType:  "https://www.googleapis.com/compute/v1/projects/" + c.project + "/zones/" + c.zone + "/acceleratorTypes/" + acceleratorType,
			},
		}
	}

	if strings.Contains(c.subnetwork, "/subnetworks/") {
		instance.NetworkInterfaces[0].Subnetwork = c.subnetwork
	} else if c.subnetwork != "" {
		instance.NetworkInterfaces[0].Subnetwork = "projects/" + c.networkProject + "/regions/" + c.region() + "/subnetworks/" + c.subnetwork
	}

	if !c.useInternalIPOnly {
		cfg := &raw.AccessConfig{
			Type: "ONE_TO_ONE_NAT",
		}
		instance.NetworkInterfaces[0].AccessConfigs = append(instance.NetworkInterfaces[0].AccessConfigs, cfg)
	}

	if c.address != "" {
		staticAddress, err := c.staticAddress()
		if err != nil {
			return err
		}

		instance.NetworkInterfaces[0].AccessConfigs[0].NatIP = staticAddress
	}

	disk, err := c.disk()
	if disk == nil || err != nil {
		instance.Disks[0].InitializeParams = &raw.AttachedDiskInitializeParams{
			DiskName:    c.diskName(),
			SourceImage: "https://www.googleapis.com/compute/v1/projects/" + d.MachineImage,
			// The maximum supported disk size is 1000GB, the cast should be fine.
			DiskSizeGb: int64(d.DiskSize),
			DiskType:   c.diskType(),
			Labels:     parseLabels(d),
		}
	} else {
		instance.Disks[0].Source = c.zoneURL + "/disks/" + c.instanceName + "-disk"
	}
	op, err := c.service.Instances.Insert(c.project, c.zone, instance).Do()

	if err != nil {
		return err
	}

	log.Infof("Waiting for Instance")
	if err = c.waitForRegionalOp(op.Name); err != nil {
		return err
	}

	instance, err = c.instance()
	if err != nil {
		return err
	}

	return c.uploadSSHKey(instance, d.GetSSHKeyPath())
}

// configureInstance configures an existing instance for use with Docker Machine.
func (c *ComputeUtil) configureInstance(d *Driver) error {
	log.Infof("Configuring instance")

	instance, err := c.instance()
	if err != nil {
		return err
	}

	if err := c.addFirewallTag(instance); err != nil {
		return err
	}

	return c.uploadSSHKey(instance, d.GetSSHKeyPath())
}

// addFirewallTag adds a tag to the instance to match the firewall rule.
func (c *ComputeUtil) addFirewallTag(instance *raw.Instance) error {
	log.Infof("Adding tag for the firewall rule")

	tags := instance.Tags
	for _, tag := range tags.Items {
		if tag == firewallTargetTag {
			return nil
		}
	}

	tags.Items = append(tags.Items, firewallTargetTag)

	op, err := c.service.Instances.SetTags(c.project, c.zone, instance.Name, tags).Do()
	if err != nil {
		return err
	}

	return c.waitForRegionalOp(op.Name)
}

// uploadSSHKey updates the instance metadata with the given ssh key.
func (c *ComputeUtil) uploadSSHKey(instance *raw.Instance, sshKeyPath string) error {
	log.Infof("Uploading SSH Key")

	sshKey, err := ioutil.ReadFile(sshKeyPath + ".pub")
	if err != nil {
		return err
	}

	metaDataValue := fmt.Sprintf("%s:%s %s\n", c.userName, strings.TrimSpace(string(sshKey)), c.userName)

	metadata := instance.Metadata
	// "sshKeys" was deprecated in favor of "ssh-keys" metadata key. However, old images may still depend
	// on the old metadata configuration. And users may still have legitimate reasons to use these older
	// images. As instance metadata is a simple key-value store, it should have no problems with having
	// the keys defined twice under two different names. Legacy images will then still be able to use the
	// legacy key naming, while new ones will get support for the expected new naming.
	metadata.Items = append(metadata.Items, &raw.MetadataItems{
		Key:   "sshKeys",
		Value: &metaDataValue,
	})
	metadata.Items = append(metadata.Items, &raw.MetadataItems{
		Key:   "ssh-keys",
		Value: &metaDataValue,
	})

	op, err := c.service.Instances.SetMetadata(c.project, c.zone, c.instanceName, metadata).Do()
	if err != nil {
		return err
	}

	return c.waitForRegionalOp(op.Name)
}

// prepareMetadata prepares instance metadata entries from provided configuration
func prepareMetadata(d *Driver) (*raw.Metadata, error) {
	metadata := &raw.Metadata{
		Items: make([]*raw.MetadataItems, 0),
	}

	for key, value := range d.Metadata {
		appendMetadata(metadata, key, value)
	}

	for key, filePath := range d.MetadataFromFile {
		value, err := readMetadataFile(filePath)
		if err != nil {
			return nil, err
		}

		appendMetadata(metadata, key, value)
	}

	return metadata, nil
}

// readMetadataFile reads the data from the provided file
func readMetadataFile(filePath string) (string, error) {
	if filePath == "" {
		return "", nil
	}

	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

// appendMetadata creates a new item from provided key and value and appends it to the provided metadata object
func appendMetadata(metadata *raw.Metadata, key string, value string) {
	if value == "" {
		return
	}

	item := &raw.MetadataItems{
		Key:   key,
		Value: &value,
	}

	metadata.Items = append(metadata.Items, item)
}

// parseTags computes the tags for the instance.
func parseTags(d *Driver) []string {
	tags := []string{firewallTargetTag}

	if d.Tags != "" {
		tags = append(tags, strings.Split(d.Tags, ",")...)
	}

	return tags
}

// parseLabels computes the tags for the instance.
func parseLabels(d *Driver) map[string]string {
	labels := map[string]string{}

	// d.Labels is an array of strings in the format "key:value"
	// also account for spaces in the format and ignore them
	for _, kv := range d.Labels {
		split := strings.SplitN(kv, ":", 2)
		key := strings.TrimSpace(split[0])

		value := ""
		if len(split) > 1 {
			value = strings.TrimSpace(split[1])
		}

		labels[key] = value
	}

	return labels
}

// deleteInstance deletes the instance, leaving the persistent disk.
func (c *ComputeUtil) deleteInstance() error {
	log.Infof("Deleting instance.")
	op, err := c.service.Instances.Delete(c.project, c.zone, c.instanceName).Do()
	if err != nil {
		return err
	}

	log.Infof("Waiting for instance to delete.")
	return c.waitForRegionalOp(op.Name)
}

// stopInstance stops the instance.
func (c *ComputeUtil) stopInstance() error {
	op, err := c.service.Instances.Stop(c.project, c.zone, c.instanceName).Do()
	if err != nil {
		return err
	}

	log.Infof("Waiting for instance to stop.")
	return c.waitForRegionalOp(op.Name)
}

// startInstance starts the instance.
func (c *ComputeUtil) startInstance() error {
	op, err := c.service.Instances.Start(c.project, c.zone, c.instanceName).Do()
	if err != nil {
		return err
	}

	log.Infof("Waiting for instance to start.")
	return c.waitForRegionalOp(op.Name)
}

// waitForOp waits for the operation to finish.
func (c *ComputeUtil) waitForOp(opGetter func() (*raw.Operation, error)) error {
	var next time.Duration

	if c.operationBackoffFactory == nil {
		return errors.New("operationBackoffFactory is not defined")
	}

	b := c.operationBackoffFactory.create()
	b.Reset()

	for {
		op, err := opGetter()
		if err != nil {
			return err
		}

		log.Debugf("Operation %q status: %s", op.Name, op.Status)
		if op.Status == "DONE" {
			if op.Error != nil {
				return fmt.Errorf("operation error: %v", *op.Error.Errors[0])
			}
			break
		}

		if next = b.NextBackOff(); next == backoff.Stop {
			return errors.New("maximum backoff elapsed time exceeded")
		}

		time.Sleep(next)
	}

	return nil
}

// waitForRegionalOp waits for the regional operation to finish.
func (c *ComputeUtil) waitForRegionalOp(name string) error {
	return c.waitForOp(func() (*raw.Operation, error) {
		return c.service.ZoneOperations.Wait(c.project, c.zone, name).Do()
	})
}

// waitForGlobalOp waits for the global operation to finish.
func (c *ComputeUtil) waitForGlobalOp(name string) error {
	return c.waitForOp(func() (*raw.Operation, error) {
		return c.service.GlobalOperations.Wait(c.project, name).Do()
	})
}

// ip retrieves and returns the external IP address of the instance.
func (c *ComputeUtil) ip() (string, error) {
	instance, err := c.service.Instances.Get(c.project, c.zone, c.instanceName).Do()
	if err != nil {
		return "", unwrapGoogleError(err)
	}

	nic := instance.NetworkInterfaces[0]
	if c.useInternalIP {
		return nic.NetworkIP, nil
	}
	return nic.AccessConfigs[0].NatIP, nil
}

func unwrapGoogleError(err error) error {
	if googleErr, ok := err.(*googleapi.Error); ok {
		return errors.New(googleErr.Message)
	}

	return err
}

func isNotFound(err error) bool {
	googleErr, ok := err.(*googleapi.Error)
	if !ok {
		return false
	}

	if googleErr.Code == http.StatusNotFound {
		return true
	}

	return false
}
