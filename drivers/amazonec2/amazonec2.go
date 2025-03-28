package amazonec2

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"

	"github.com/docker/machine/drivers/driverutil"
	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/log"
	"github.com/docker/machine/libmachine/mcnflag"
	"github.com/docker/machine/libmachine/mcnutils"
	"github.com/docker/machine/libmachine/ssh"
	"github.com/docker/machine/libmachine/state"
)

const (
	driverName                           = "amazonec2"
	ipRange                              = "0.0.0.0/0"
	machineSecurityGroupName             = "docker-machine"
	defaultAmiId                         = "ami-08d4ac5b634553e16"
	defaultRegion                        = "us-east-1"
	defaultInstanceType                  = "t2.micro"
	defaultDeviceName                    = "/dev/sda1"
	defaultRootSize                      = 16
	defaultVolumeType                    = "gp2"
	gp3MinimalVolumeIops                 = 3000
	gp3MinimalVolumeThroughput           = 125
	defaultZone                          = "a"
	defaultSecurityGroup                 = machineSecurityGroupName
	defaultSSHPort                       = 22
	defaultSSHUser                       = "ubuntu"
	defaultSpotPrice                     = "0.50"
	defaultBlockDurationMinutes          = 0
	defaultMetadataTokenSetting          = "optional"
	defaultMetadataTokenResponseHopLimit = 1
)

const (
	keypairNotFoundCode = "InvalidKeyPair.NotFound"
)

var (
	dockerPort                           = 2376
	swarmPort                            = 3376
	errorNoPrivateSSHKey                 = errors.New("using --amazonec2-keypair-name also requires --amazonec2-ssh-keypath")
	errorMissingCredentials              = errors.New("amazonec2 driver requires AWS credentials configured with the --amazonec2-access-key and --amazonec2-secret-key options, environment variables, ~/.aws/credentials, or an instance role")
	errorNoVPCIdFound                    = errors.New("amazonec2 driver requires either the --amazonec2-subnet-id or --amazonec2-vpc-id option or an AWS Account with a default vpc-id")
	errorNoSubnetsFound                  = errors.New("The desired subnet could not be located in this region. Is '--amazonec2-subnet-id' or AWS_SUBNET_ID configured correctly?")
	errorDisableSSLWithoutCustomEndpoint = errors.New("using --amazonec2-insecure-transport also requires --amazonec2-endpoint")
	errorReadingUserData                 = errors.New("unable to read --amazonec2-userdata file")
	errorUnprocessableResponse           = errors.New("unexpected AWS API response")
)

type Driver struct {
	*drivers.BaseDriver
	clientFactory         func() Ec2Client
	awsCredentialsFactory func() awsCredentials
	Id                    string
	AccessKey             string
	SecretKey             string
	SessionToken          string
	Region                string
	AMI                   string
	SSHKeyID              int
	// ExistingKey keeps track of whether the key was created by us or we used an existing one. If an existing one was used, we shouldn't delete it when the machine is deleted.
	ExistingKey      bool
	KeyName          string
	InstanceId       string
	InstanceType     string
	PrivateIPAddress string

	// NB: SecurityGroupId expanded from single value to slice on 26 Feb 2016 - we maintain both for host storage backwards compatibility.
	SecurityGroupId  string
	SecurityGroupIds []string

	// NB: SecurityGroupName expanded from single value to slice on 26 Feb 2016 - we maintain both for host storage backwards compatibility.
	SecurityGroupName  string
	SecurityGroupNames []string

	SecurityGroupReadOnly         bool
	OpenPorts                     []string
	Tags                          string
	ReservationId                 string
	DeviceName                    string
	RootSize                      int64
	VolumeType                    string
	VolumeIops                    int64
	VolumeThroughput              int64
	VolumeEncrypted               bool
	VolumeKmsKeyId                string
	IamInstanceProfile            string
	VpcId                         string
	SubnetId                      string
	Zone                          string
	keyPath                       string
	RequestSpotInstance           bool
	SpotPrice                     string
	BlockDurationMinutes          int64
	PrivateIPOnly                 bool
	UsePrivateIP                  bool
	UseEbsOptimizedInstance       bool
	Monitoring                    bool
	SSHPrivateKeyPath             string
	RetryCount                    int
	Endpoint                      string
	DisableSSL                    bool
	UserDataFile                  string
	MetadataTokenSetting          string
	MetadataTokenResponseHopLimit int64
	CreditSpecification           string
}

type clientFactory interface {
	build(d *Driver) Ec2Client
}

func (d *Driver) GetCreateFlags() []mcnflag.Flag {
	return []mcnflag.Flag{
		mcnflag.StringFlag{
			Name:   "amazonec2-access-key",
			Usage:  "AWS Access Key",
			EnvVar: "AWS_ACCESS_KEY_ID",
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-secret-key",
			Usage:  "AWS Secret Key",
			EnvVar: "AWS_SECRET_ACCESS_KEY",
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-session-token",
			Usage:  "AWS Session Token",
			EnvVar: "AWS_SESSION_TOKEN",
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-ami",
			Usage:  "AWS machine image",
			EnvVar: "AWS_AMI",
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-region",
			Usage:  "AWS region",
			Value:  defaultRegion,
			EnvVar: "AWS_DEFAULT_REGION",
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-vpc-id",
			Usage:  "AWS VPC id",
			EnvVar: "AWS_VPC_ID",
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-zone",
			Usage:  "AWS zone for instance (i.e. a,b,c,d,e)",
			Value:  defaultZone,
			EnvVar: "AWS_ZONE",
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-subnet-id",
			Usage:  "AWS VPC subnet id",
			EnvVar: "AWS_SUBNET_ID",
		},
		mcnflag.BoolFlag{
			Name:   "amazonec2-security-group-readonly",
			Usage:  "Skip adding default rules to security groups",
			EnvVar: "AWS_SECURITY_GROUP_READONLY",
		},
		mcnflag.StringSliceFlag{
			Name:   "amazonec2-security-group",
			Usage:  "AWS VPC security group",
			Value:  []string{defaultSecurityGroup},
			EnvVar: "AWS_SECURITY_GROUP",
		},
		mcnflag.StringSliceFlag{
			Name:  "amazonec2-open-port",
			Usage: "Make the specified port number accessible from the Internet",
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-tags",
			Usage:  "AWS Tags (e.g. key1,value1,key2,value2)",
			EnvVar: "AWS_TAGS",
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-instance-type",
			Usage:  "AWS instance type",
			Value:  defaultInstanceType,
			EnvVar: "AWS_INSTANCE_TYPE",
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-device-name",
			Usage:  "AWS root device name",
			Value:  defaultDeviceName,
			EnvVar: "AWS_DEVICE_NAME",
		},
		mcnflag.IntFlag{
			Name:   "amazonec2-root-size",
			Usage:  "AWS root disk size (in GB)",
			Value:  defaultRootSize,
			EnvVar: "AWS_ROOT_SIZE",
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-volume-type",
			Usage:  "Amazon EBS volume type",
			Value:  defaultVolumeType,
			EnvVar: "AWS_VOLUME_TYPE",
		},
		mcnflag.IntFlag{
			Name:   "amazonec2-volume-iops",
			Usage:  "AWS EBS volume IOPS",
			EnvVar: "AWS_VOLUME_IOPS",
		},
		mcnflag.IntFlag{
			Name:   "amazonec2-volume-throughput",
			Usage:  "AWS EBS volume throughput",
			EnvVar: "AWS_VOLUME_THROUGHPUT",
		},
		mcnflag.BoolFlag{
			Name:  "amazonec2-volume-encrypted",
			Usage: "Set this flag to encrypt storage volume",
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-volume-kms-key",
			Usage:  "Amazon EBS KMS key id/arn used to encrypt volume",
			EnvVar: "AWS_VOLUME_KMS_KEY",
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-iam-instance-profile",
			Usage:  "AWS IAM Instance Profile",
			EnvVar: "AWS_INSTANCE_PROFILE",
		},
		mcnflag.IntFlag{
			Name:   "amazonec2-ssh-port",
			Usage:  "SSH port",
			Value:  defaultSSHPort,
			EnvVar: "AWS_SSH_PORT",
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-ssh-user",
			Usage:  "SSH username",
			Value:  defaultSSHUser,
			EnvVar: "AWS_SSH_USER",
		},
		mcnflag.BoolFlag{
			Name:  "amazonec2-request-spot-instance",
			Usage: "Set this flag to request spot instance",
		},
		mcnflag.StringFlag{
			Name:  "amazonec2-spot-price",
			Usage: "AWS spot instance bid price (in dollar)",
			Value: defaultSpotPrice,
		},
		mcnflag.IntFlag{
			Name:  "amazonec2-block-duration-minutes",
			Usage: "AWS spot instance duration in minutes (60, 120, 180, 240, 300, or 360)",
			Value: defaultBlockDurationMinutes,
		},
		mcnflag.BoolFlag{
			Name:  "amazonec2-private-address-only",
			Usage: "Only use a private IP address",
		},
		mcnflag.BoolFlag{
			Name:  "amazonec2-use-private-address",
			Usage: "Force the usage of private IP address",
		},
		mcnflag.BoolFlag{
			Name:  "amazonec2-monitoring",
			Usage: "Set this flag to enable CloudWatch monitoring",
		},
		mcnflag.BoolFlag{
			Name:  "amazonec2-use-ebs-optimized-instance",
			Usage: "Create an EBS optimized instance",
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-ssh-keypath",
			Usage:  "SSH Key for Instance",
			EnvVar: "AWS_SSH_KEYPATH",
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-keypair-name",
			Usage:  "AWS keypair to use; requires --amazonec2-ssh-keypath",
			EnvVar: "AWS_KEYPAIR_NAME",
		},
		mcnflag.IntFlag{
			Name:  "amazonec2-retries",
			Usage: "Set retry count for recoverable failures (use -1 to disable)",
			Value: 5,
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-endpoint",
			Usage:  "Optional endpoint URL (hostname only or fully qualified URI)",
			Value:  "",
			EnvVar: "AWS_ENDPOINT",
		},
		mcnflag.BoolFlag{
			Name:   "amazonec2-insecure-transport",
			Usage:  "Disable SSL when sending requests",
			EnvVar: "AWS_INSECURE_TRANSPORT",
		},
		mcnflag.StringFlag{
			Name:   "amazonec2-userdata",
			Usage:  "path to file with cloud-init user data",
			EnvVar: "AWS_USERDATA",
		},
		mcnflag.StringFlag{
			Name:  "amazonec2-metadata-token",
			Usage: "Whether the metadata token is required or optional",
			Value: defaultMetadataTokenSetting,
		},
		mcnflag.IntFlag{
			Name:  "amazonec2-metadata-token-response-hop-limit",
			Usage: "The number of network hops that the metadata token can travel",
			Value: defaultMetadataTokenResponseHopLimit,
		},
		mcnflag.StringFlag{
			Name:  "amazonec2-credit-specification",
			Usage: "The credit option for CPU usage (unlimited or standard)",
		},
	}
}

func NewDriver(hostName, storePath string) *Driver {
	id := generateId()
	driver := &Driver{
		Id:                   id,
		AMI:                  defaultAmiId,
		Region:               defaultRegion,
		InstanceType:         defaultInstanceType,
		RootSize:             defaultRootSize,
		Zone:                 defaultZone,
		SecurityGroupNames:   []string{defaultSecurityGroup},
		SpotPrice:            defaultSpotPrice,
		BlockDurationMinutes: defaultBlockDurationMinutes,
		BaseDriver: &drivers.BaseDriver{
			SSHPort:     defaultSSHPort,
			SSHUser:     defaultSSHUser,
			MachineName: hostName,
			StorePath:   storePath,
		},
		MetadataTokenSetting:          defaultMetadataTokenSetting,
		MetadataTokenResponseHopLimit: defaultMetadataTokenResponseHopLimit,
	}

	driver.clientFactory = driver.buildClient
	driver.awsCredentialsFactory = driver.buildCredentials

	return driver
}

func (d *Driver) buildClient() Ec2Client {
	config := aws.NewConfig()
	alogger := AwsLogger()
	config = config.WithRegion(d.Region)
	config = config.WithCredentials(d.awsCredentialsFactory().Credentials())
	config = config.WithLogger(alogger)
	config = config.WithLogLevel(aws.LogDebugWithHTTPBody)
	config = config.WithMaxRetries(d.RetryCount)
	if d.Endpoint != "" {
		config = config.WithEndpoint(d.Endpoint)
		config = config.WithDisableSSL(d.DisableSSL)
	}
	return ec2.New(session.New(config))
}

func (d *Driver) buildCredentials() awsCredentials {
	return NewAWSCredentials(d.AccessKey, d.SecretKey, d.SessionToken)
}

func (d *Driver) getClient() Ec2Client {
	return d.clientFactory()
}

func (d *Driver) SetConfigFromFlags(flags drivers.DriverOptions) error {
	d.Endpoint = flags.String("amazonec2-endpoint")

	region, err := validateAwsRegion(flags.String("amazonec2-region"))
	if err != nil && d.Endpoint == "" {
		return err
	}

	image := flags.String("amazonec2-ami")
	if len(image) == 0 {
		image = regionDetails[region].AmiId
	}

	d.AccessKey = flags.String("amazonec2-access-key")
	d.SecretKey = flags.String("amazonec2-secret-key")
	d.SessionToken = flags.String("amazonec2-session-token")
	d.Region = region
	d.AMI = image
	d.RequestSpotInstance = flags.Bool("amazonec2-request-spot-instance")
	d.SpotPrice = flags.String("amazonec2-spot-price")
	d.BlockDurationMinutes = int64(flags.Int("amazonec2-block-duration-minutes"))
	d.InstanceType = flags.String("amazonec2-instance-type")
	d.VpcId = flags.String("amazonec2-vpc-id")
	d.SubnetId = flags.String("amazonec2-subnet-id")
	d.SecurityGroupNames = flags.StringSlice("amazonec2-security-group")
	d.SecurityGroupReadOnly = flags.Bool("amazonec2-security-group-readonly")
	d.Tags = flags.String("amazonec2-tags")
	zone := flags.String("amazonec2-zone")
	d.Zone = zone[:]
	d.DeviceName = flags.String("amazonec2-device-name")
	d.RootSize = int64(flags.Int("amazonec2-root-size"))
	d.VolumeType = flags.String("amazonec2-volume-type")
	d.VolumeIops = int64(flags.Int("amazonec2-volume-iops"))
	d.VolumeThroughput = int64(flags.Int("amazonec2-volume-throughput"))
	d.VolumeEncrypted = flags.Bool("amazonec2-volume-encrypted")
	d.VolumeKmsKeyId = flags.String("amazonec2-volume-kms-key")
	d.IamInstanceProfile = flags.String("amazonec2-iam-instance-profile")
	d.SSHUser = flags.String("amazonec2-ssh-user")
	d.SSHPort = flags.Int("amazonec2-ssh-port")
	d.PrivateIPOnly = flags.Bool("amazonec2-private-address-only")
	d.UsePrivateIP = flags.Bool("amazonec2-use-private-address")
	d.Monitoring = flags.Bool("amazonec2-monitoring")
	d.UseEbsOptimizedInstance = flags.Bool("amazonec2-use-ebs-optimized-instance")
	d.SSHPrivateKeyPath = flags.String("amazonec2-ssh-keypath")
	d.KeyName = flags.String("amazonec2-keypair-name")
	d.ExistingKey = flags.String("amazonec2-keypair-name") != ""
	d.SetSwarmConfigFromFlags(flags)
	d.RetryCount = flags.Int("amazonec2-retries")
	d.OpenPorts = flags.StringSlice("amazonec2-open-port")
	d.UserDataFile = flags.String("amazonec2-userdata")
	d.MetadataTokenSetting = flags.String("amazonec2-metadata-token")
	d.MetadataTokenResponseHopLimit = int64(flags.Int("amazonec2-metadata-token-response-hop-limit"))
	d.DisableSSL = flags.Bool("amazonec2-insecure-transport")
	d.CreditSpecification = strings.TrimSpace(flags.String("amazonec2-credit-specification"))

	if d.DisableSSL && d.Endpoint == "" {
		return errorDisableSSLWithoutCustomEndpoint
	}

	if d.KeyName != "" && d.SSHPrivateKeyPath == "" {
		return errorNoPrivateSSHKey
	}

	_, err = d.awsCredentialsFactory().Credentials().Get()
	if err != nil {
		return errorMissingCredentials
	}

	if d.VpcId == "" {
		d.VpcId, err = d.getDefaultVPCId()
		if err != nil {
			log.Warnf("Couldn't determine your account Default VPC ID : %q", err)
		}
	}

	if d.SubnetId == "" && d.VpcId == "" {
		return errorNoVPCIdFound
	}

	if d.SubnetId != "" && d.VpcId != "" {
		subnetFilter := []*ec2.Filter{
			{
				Name:   aws.String("subnet-id"),
				Values: []*string{&d.SubnetId},
			},
		}

		subnets, err := d.getClient().DescribeSubnets(&ec2.DescribeSubnetsInput{
			Filters: subnetFilter,
		})
		if err != nil {
			return err
		}

		if subnets == nil || len(subnets.Subnets) == 0 {
			return errorNoSubnetsFound
		}

		if *subnets.Subnets[0].VpcId != d.VpcId {
			return fmt.Errorf("SubnetId: %s does not belong to VpcId: %s", d.SubnetId, d.VpcId)
		}
	}

	if d.isSwarmMaster() {
		u, err := url.Parse(d.SwarmHost)
		if err != nil {
			return fmt.Errorf("error parsing swarm host: %s", err)
		}

		parts := strings.Split(u.Host, ":")
		port, err := strconv.Atoi(parts[1])
		if err != nil {
			return err
		}

		swarmPort = port
	}

	return nil
}

// DriverName returns the name of the driver
func (d *Driver) DriverName() string {
	return driverName
}

func (d *Driver) checkPrereqs() error {
	// check for existing keypair
	keyName := d.KeyName
	keyShouldExist := true
	if keyName == "" {
		keyName = d.MachineName
		keyShouldExist = false
	}

	key, err := d.getClient().DescribeKeyPairs(&ec2.DescribeKeyPairsInput{
		KeyNames: []*string{&keyName},
	})
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			if awsErr.Code() == keypairNotFoundCode && keyShouldExist {
				return fmt.Errorf("There is no keypair with the name %s. Please verify the key name provided.", keyName)
			}
			if awsErr.Code() == keypairNotFoundCode && !keyShouldExist {
				// Not a real error for 'NotFound' since we're checking existence
			}
		} else {
			return err
		}
	}

	// In case we got a result with an empty set of keys
	if err == nil && len(key.KeyPairs) != 0 {
		if !keyShouldExist {
			return fmt.Errorf("There is already a keypair with the name %s.  Please either remove that keypair or use a different machine name.", d.MachineName)
		}
		// otherwise we found the key: success
	}

	regionZone := d.getRegionZone()
	if d.SubnetId == "" {
		filters := []*ec2.Filter{
			{
				Name:   aws.String("availability-zone"),
				Values: []*string{&regionZone},
			},
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{&d.VpcId},
			},
		}

		subnets, err := d.getClient().DescribeSubnets(&ec2.DescribeSubnetsInput{
			Filters: filters,
		})
		if err != nil {
			return err
		}

		if len(subnets.Subnets) == 0 {
			return fmt.Errorf("unable to find a subnet that is both in the zone %s and belonging to VPC ID %s", regionZone, d.VpcId)
		}

		d.SubnetId = *subnets.Subnets[0].SubnetId

		// try to find default
		if len(subnets.Subnets) > 1 {
			for _, subnet := range subnets.Subnets {
				if subnet.DefaultForAz != nil && *subnet.DefaultForAz {
					d.SubnetId = *subnet.SubnetId
					break
				}
			}
		}
	}

	return nil
}

func (d *Driver) PreCreateCheck() error {
	return d.checkPrereqs()
}

func (d *Driver) instanceIpAvailable() bool {
	ip, err := d.GetIP()
	if err != nil {
		log.Debug(err)
	}
	if ip != "" {
		d.IPAddress = ip
		log.Debugf("Got the IP Address, it's %q", d.IPAddress)
		return true
	}
	return false
}

func makePointerSlice(stackSlice []string) []*string {
	pointerSlice := []*string{}
	for i := range stackSlice {
		pointerSlice = append(pointerSlice, &stackSlice[i])
	}
	return pointerSlice
}

// Support migrating single string Driver fields to slices.
func migrateStringToSlice(value string, values []string) (result []string) {
	if value != "" {
		result = append(result, value)
	}
	result = append(result, values...)
	return
}

func (d *Driver) securityGroupNames() (ids []string) {
	return migrateStringToSlice(d.SecurityGroupName, d.SecurityGroupNames)
}

func (d *Driver) securityGroupIds() (ids []string) {
	return migrateStringToSlice(d.SecurityGroupId, d.SecurityGroupIds)
}

func (d *Driver) Base64UserData() (userdata string, err error) {
	if d.UserDataFile != "" {
		buf, ioerr := ioutil.ReadFile(d.UserDataFile)
		if ioerr != nil {
			log.Warnf("failed to read user data file %q: %s", d.UserDataFile, ioerr)
			err = errorReadingUserData
			return
		}
		userdata = base64.StdEncoding.EncodeToString(buf)
	}
	return
}

func (d *Driver) Create() error {
	if err := d.checkPrereqs(); err != nil {
		return err
	}

	if err := d.innerCreate(); err != nil {
		// cleanup partially created resources
		d.Remove()
		return err
	}

	return nil
}

func (d *Driver) innerCreate() error {
	log.Infof("Launching instance...")
	if err := d.createKeyPair(); err != nil {
		return fmt.Errorf("unable to create key pair: %s", err)
	}

	if err := d.configureSecurityGroups(d.securityGroupNames()); err != nil {
		return err
	}

	var userdata string
	if b64, err := d.Base64UserData(); err != nil {
		return err
	} else {
		userdata = b64
	}

	ebsVolume := &ec2.EbsBlockDevice{
		VolumeSize:          aws.Int64(d.RootSize),
		VolumeType:          aws.String(d.VolumeType),
		Encrypted:           aws.Bool(d.VolumeEncrypted),
		DeleteOnTermination: aws.Bool(true),
	}

	switch d.VolumeType {
	case "io1", "io2":
		ebsVolume.Iops = aws.Int64(d.VolumeIops)
	case "gp3":
		ebsVolume.Iops = aws.Int64(clamp(d.VolumeIops, gp3MinimalVolumeIops, math.MaxInt64))
		ebsVolume.Throughput = aws.Int64(clamp(d.VolumeThroughput, gp3MinimalVolumeThroughput, math.MaxInt64))
	}

	bdm := &ec2.BlockDeviceMapping{
		DeviceName: aws.String(d.DeviceName),
		Ebs:        ebsVolume,
	}
	if d.VolumeKmsKeyId != "" {
		bdm.Ebs.KmsKeyId = aws.String(d.VolumeKmsKeyId)
	}
	netSpecs := []*ec2.InstanceNetworkInterfaceSpecification{{
		DeviceIndex:              aws.Int64(0), // eth0
		Groups:                   makePointerSlice(d.securityGroupIds()),
		SubnetId:                 &d.SubnetId,
		AssociatePublicIpAddress: aws.Bool(!d.PrivateIPOnly),
	}}

	regionZone := d.getRegionZone()
	log.Debugf("launching instance in subnet %s", d.SubnetId)

	var instance *ec2.Instance

	// As mentioned in
	// https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/spot-requests.html#concepts-spot-instances-request-tags,
	// when you tag a Spot Instance request, the instances and volumes that are launched by the Spot Instance request
	// are not automatically tagged. We must explicitly tag the instances and volumes
	// launched by the Spot Instance request once they are available
	req := ec2.RunInstancesInput{
		ImageId:  &d.AMI,
		MinCount: aws.Int64(1),
		MaxCount: aws.Int64(1),
		Placement: &ec2.Placement{
			AvailabilityZone: &regionZone,
		},
		KeyName:           &d.KeyName,
		InstanceType:      &d.InstanceType,
		NetworkInterfaces: netSpecs,
		MetadataOptions: &ec2.InstanceMetadataOptionsRequest{
			HttpTokens:              &d.MetadataTokenSetting,
			HttpPutResponseHopLimit: &d.MetadataTokenResponseHopLimit,
		},
		Monitoring: &ec2.RunInstancesMonitoringEnabled{Enabled: aws.Bool(d.Monitoring)},
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{
			Name: &d.IamInstanceProfile,
		},
		EbsOptimized:        &d.UseEbsOptimizedInstance,
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{bdm},
		UserData:            &userdata,
	}
	if d.CreditSpecification != "" {
		req.CreditSpecification = &ec2.CreditSpecificationRequest{
			CpuCredits: &d.CreditSpecification,
		}
	}
	if d.RequestSpotInstance {
		req.InstanceMarketOptions = &ec2.InstanceMarketOptionsRequest{
			MarketType: aws.String("spot"),
			SpotOptions: &ec2.SpotMarketOptions{
				MaxPrice: &d.SpotPrice,
			},
		}
		if d.BlockDurationMinutes != 0 {
			req.InstanceMarketOptions.SpotOptions.BlockDurationMinutes = &d.BlockDurationMinutes
		}
	} else {
		req.TagSpecifications = d.createTagSpecifications()
	}

	inst, err := d.getClient().RunInstances(&req)

	if err != nil {
		return fmt.Errorf("Error launching instance: %v", err)
	}
	if inst == nil || len(inst.Instances) < 1 {
		return fmt.Errorf("error launching instance: %v", errorUnprocessableResponse)
	}
	instance = inst.Instances[0]

	d.InstanceId = *instance.InstanceId

	if d.RequestSpotInstance && d.InstanceId != "" {
		// According to this AWS Example,
		// https://docs.aws.amazon.com/sdk-for-go/v1/developer-guide/ec2-example-create-images.html
		// You can add up to 10 tags to an instance in a single CreateTags operation
		tags := d.createTags()
		for {
			end := int(math.Min(float64(10), float64(len(tags))))
			t := tags[:end]

			req := ec2.CreateTagsInput{
				Resources: []*string{instance.InstanceId},
				Tags:      t,
			}
			_, err := d.getClient().CreateTags(&req)
			if err != nil {
				return fmt.Errorf("Error setting tags for the spot instance: %v", err)
			}

			tags = tags[end:]

			if len(tags) == 0 {
				break
			}
		}
	}

	log.Debug("waiting for ip address to become available")
	if err := mcnutils.WaitFor(d.instanceIpAvailable); err != nil {
		return err
	}

	if instance.PrivateIpAddress != nil {
		d.PrivateIPAddress = *instance.PrivateIpAddress
	}

	d.waitForInstance()

	log.Debugf("created instance ID %s, IP address %s, Private IP address %s",
		d.InstanceId,
		d.IPAddress,
		d.PrivateIPAddress,
	)

	return nil
}

func (d *Driver) GetURL() (string, error) {
	if err := drivers.MustBeRunning(d); err != nil {
		return "", err
	}

	ip, err := d.GetIP()
	if err != nil {
		return "", err
	}
	if ip == "" {
		return "", nil
	}

	return fmt.Sprintf("tcp://%s", net.JoinHostPort(ip, strconv.Itoa(dockerPort))), nil
}

func (d *Driver) GetIP() (string, error) {
	if d.IPAddress != "" {
		return d.IPAddress, nil
	}

	inst, err := d.getInstance()
	if err != nil {
		return "", err
	}

	if d.PrivateIPOnly {
		if inst.PrivateIpAddress == nil {
			return "", fmt.Errorf("No private IP for instance %v", *inst.InstanceId)
		}
		return *inst.PrivateIpAddress, nil
	}

	if d.UsePrivateIP {
		if inst.PrivateIpAddress == nil {
			return "", fmt.Errorf("No private IP for instance %v", *inst.InstanceId)
		}
		return *inst.PrivateIpAddress, nil
	}

	if inst.PublicIpAddress == nil {
		return "", fmt.Errorf("No IP for instance %v", *inst.InstanceId)
	}
	return *inst.PublicIpAddress, nil
}

func (d *Driver) GetState() (state.State, error) {
	inst, err := d.getInstance()
	if err != nil {
		return state.Error, err
	}
	switch *inst.State.Name {
	case ec2.InstanceStateNamePending:
		return state.Starting, nil
	case ec2.InstanceStateNameRunning:
		return state.Running, nil
	case ec2.InstanceStateNameStopping:
		return state.Stopping, nil
	case ec2.InstanceStateNameShuttingDown:
		return state.Stopping, nil
	case ec2.InstanceStateNameStopped:
		return state.Stopped, nil
	case ec2.InstanceStateNameTerminated:
		return state.Error, nil
	default:
		log.Warnf("unrecognized instance state: %v", *inst.State.Name)
		return state.Error, nil
	}
}

func (d *Driver) GetSSHHostname() (string, error) {
	// TODO: use @nathanleclaire retry func here (ehazlett)
	return d.GetIP()
}

func (d *Driver) GetSSHPort() (int, error) {
	if d.SSHPort == 0 {
		d.SSHPort = defaultSSHPort
	}

	return d.SSHPort, nil
}

func (d *Driver) GetSSHUsername() string {
	if d.SSHUser == "" {
		d.SSHUser = defaultSSHUser
	}

	return d.SSHUser
}

func (d *Driver) getEbsVolumeId() (string, error) {
	inst, err := d.getInstance()
	if err != nil {
		return "", err
	}

	if len(inst.BlockDeviceMappings) == 0 || inst.BlockDeviceMappings[0].Ebs == nil {
		return "", nil
	}

	return *inst.BlockDeviceMappings[0].Ebs.VolumeId, nil
}

func (d *Driver) Start() error {
	_, err := d.getClient().StartInstances(&ec2.StartInstancesInput{
		InstanceIds: []*string{&d.InstanceId},
	})
	if err != nil {
		return err
	}

	return d.waitForInstance()
}

func (d *Driver) Stop() error {
	_, err := d.getClient().StopInstances(&ec2.StopInstancesInput{
		InstanceIds: []*string{&d.InstanceId},
		Force:       aws.Bool(false),
	})
	return err
}

func (d *Driver) Restart() error {
	_, err := d.getClient().RebootInstances(&ec2.RebootInstancesInput{
		InstanceIds: []*string{&d.InstanceId},
	})
	return err
}

func (d *Driver) Kill() error {
	_, err := d.getClient().StopInstances(&ec2.StopInstancesInput{
		InstanceIds: []*string{&d.InstanceId},
		Force:       aws.Bool(true),
	})
	return err
}

func (d *Driver) Remove() error {
	multierr := mcnutils.MultiError{
		Errs: []error{},
	}

	if err := d.terminate(); err != nil {
		multierr.Errs = append(multierr.Errs, err)
	}

	if !d.ExistingKey {
		if err := d.deleteKeyPair(); err != nil {
			multierr.Errs = append(multierr.Errs, err)
		}
	}

	if len(multierr.Errs) == 0 {
		return nil
	}

	return multierr
}

func (d *Driver) getInstance() (*ec2.Instance, error) {
	instances, err := d.getClient().DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{&d.InstanceId},
	})
	if err != nil {
		return nil, err
	}
	if instances == nil || len(instances.Reservations) < 1 || len(instances.Reservations[0].Instances) < 1 {
		return nil, fmt.Errorf("error getting instance: %v", errorUnprocessableResponse)
	}

	return instances.Reservations[0].Instances[0], nil
}

func (d *Driver) instanceIsRunning() bool {
	st, err := d.GetState()
	if err != nil {
		log.Debug(err)
	}
	if st == state.Running {
		return true
	}
	return false
}

func (d *Driver) waitForInstance() error {
	if err := mcnutils.WaitFor(d.instanceIsRunning); err != nil {
		return err
	}

	return nil
}

func (d *Driver) createKeyPair() error {
	keyPath := ""

	if d.SSHPrivateKeyPath == "" {
		log.Debugf("Creating New SSH Key")
		if err := ssh.GenerateSSHKey(d.GetSSHKeyPath()); err != nil {
			return err
		}
		keyPath = d.GetSSHKeyPath()
	} else {
		log.Debugf("Using SSHPrivateKeyPath: %s", d.SSHPrivateKeyPath)
		if err := mcnutils.CopyFile(d.SSHPrivateKeyPath, d.GetSSHKeyPath()); err != nil {
			return err
		}
		if err := mcnutils.CopyFile(d.SSHPrivateKeyPath+".pub", d.GetSSHKeyPath()+".pub"); err != nil {
			return err
		}
		if d.KeyName != "" {
			log.Debugf("Using existing EC2 key pair: %s", d.KeyName)
			return nil
		}
		keyPath = d.SSHPrivateKeyPath
	}

	publicKey, err := ioutil.ReadFile(keyPath + ".pub")
	if err != nil {
		return err
	}

	keyName := d.MachineName

	log.Debugf("creating key pair: %s", keyName)
	_, err = d.getClient().ImportKeyPair(&ec2.ImportKeyPairInput{
		KeyName:           &keyName,
		PublicKeyMaterial: publicKey,
	})
	if err != nil {
		return err
	}
	d.KeyName = keyName
	return nil
}

func (d *Driver) terminate() error {
	if d.InstanceId == "" {
		log.Warn("Missing instance ID, this is likely due to a failure during machine creation")
		return nil
	}

	log.Debugf("terminating instance: %s", d.InstanceId)
	_, err := d.getClient().TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: []*string{&d.InstanceId},
	})

	if err != nil {
		if strings.HasPrefix(err.Error(), "unknown instance") ||
			strings.HasPrefix(err.Error(), "InvalidInstanceID.NotFound") {
			log.Warn("Remote instance does not exist, proceeding with removing local reference")
			return nil
		}

		return fmt.Errorf("unable to terminate instance: %s", err)
	}
	return nil
}

func (d *Driver) isSwarmMaster() bool {
	return d.SwarmMaster
}

func (d *Driver) securityGroupAvailableFunc(id string) func() bool {
	return func() bool {

		securityGroup, err := d.getClient().DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
			GroupIds: []*string{&id},
		})
		if err == nil && len(securityGroup.SecurityGroups) > 0 {
			return true
		} else if err == nil {
			log.Debugf("No security group with id %v found", id)
			return false
		}
		log.Debug(err)
		return false
	}
}

func (d *Driver) createTagSpecifications() []*ec2.TagSpecification {
	tags := d.createTags()
	return []*ec2.TagSpecification{
		{
			ResourceType: aws.String("instance"),
			Tags:         tags,
		},
		{
			ResourceType: aws.String("volume"),
			Tags:         tags,
		},
		{
			ResourceType: aws.String("network-interface"),
			Tags:         tags,
		},
	}
}

func (d *Driver) createTags() []*ec2.Tag {
	var tags []*ec2.Tag
	tags = append(tags, &ec2.Tag{
		Key:   aws.String("Name"),
		Value: &d.MachineName,
	})

	if d.Tags != "" {
		t := strings.Split(d.Tags, ",")
		if len(t) > 0 && len(t)%2 != 0 {
			log.Warnf("Tags are not key value in pairs. %d elements found", len(t))
		}
		for i := 0; i < len(t)-1; i += 2 {
			tags = append(tags, &ec2.Tag{
				Key:   &t[i],
				Value: &t[i+1],
			})
		}
	}

	return tags
}

func (d *Driver) getTagResources() []*string {
	resources := []*string{&d.InstanceId}

	volumeId, err := d.getEbsVolumeId()
	if err != nil {
		log.Warnf("failed to get EBS volume ID: %s", err)

		return resources
	}

	return append(resources, &volumeId)
}

func (d *Driver) configureSecurityGroups(groupNames []string) error {
	if len(groupNames) == 0 {
		log.Debugf("no security groups to configure in %s", d.VpcId)
		return nil
	}

	log.Debugf("configuring security groups in %s", d.VpcId)

	filters := []*ec2.Filter{
		{
			Name:   aws.String("group-name"),
			Values: makePointerSlice(groupNames),
		},
		{
			Name:   aws.String("vpc-id"),
			Values: []*string{&d.VpcId},
		},
	}
	groups, err := d.getClient().DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: filters,
	})
	if err != nil {
		return err
	}

	var groupsByName = make(map[string]*ec2.SecurityGroup)
	for _, securityGroup := range groups.SecurityGroups {
		groupsByName[*securityGroup.GroupName] = securityGroup
	}

	for _, groupName := range groupNames {
		var group *ec2.SecurityGroup
		securityGroup, ok := groupsByName[groupName]
		if ok {
			log.Debugf("found existing security group (%s) in %s", groupName, d.VpcId)
			group = securityGroup
		} else {
			log.Debugf("creating security group (%s) in %s", groupName, d.VpcId)
			groupResp, err := d.getClient().CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
				GroupName:   aws.String(groupName),
				Description: aws.String("Docker Machine"),
				VpcId:       aws.String(d.VpcId),
			})
			if err != nil {
				return err
			}
			// Manually translate into the security group construct
			group = &ec2.SecurityGroup{
				GroupId:   groupResp.GroupId,
				VpcId:     aws.String(d.VpcId),
				GroupName: aws.String(groupName),
			}
			// wait until created (dat eventual consistency)
			log.Debugf("waiting for group (%s) to become available", *group.GroupId)
			if err := mcnutils.WaitFor(d.securityGroupAvailableFunc(*group.GroupId)); err != nil {
				return err
			}
		}
		d.SecurityGroupIds = append(d.SecurityGroupIds, *group.GroupId)

		perms, err := d.configureSecurityGroupPermissions(group)
		if err != nil {
			return err
		}

		if len(perms) != 0 {
			log.Debugf("authorizing group %s with permissions: %v", groupNames, perms)
			_, err := d.getClient().AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
				GroupId:       group.GroupId,
				IpPermissions: perms,
			})
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (d *Driver) configureSecurityGroupPermissions(group *ec2.SecurityGroup) ([]*ec2.IpPermission, error) {
	if d.SecurityGroupReadOnly {
		log.Debug("Skipping permission configuration on security groups")
		return nil, nil
	}
	hasPorts := make(map[string]bool)
	for _, p := range group.IpPermissions {
		if p.FromPort != nil {
			hasPorts[fmt.Sprintf("%d/%s", *p.FromPort, *p.IpProtocol)] = true
		}
	}

	perms := []*ec2.IpPermission{}

	if !hasPorts[fmt.Sprintf("%d/tcp", d.BaseDriver.SSHPort)] {
		perms = append(perms, &ec2.IpPermission{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(int64(d.BaseDriver.SSHPort)),
			ToPort:     aws.Int64(int64(d.BaseDriver.SSHPort)),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(ipRange)}},
		})
	}

	if !hasPorts[fmt.Sprintf("%d/tcp", dockerPort)] {
		perms = append(perms, &ec2.IpPermission{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(int64(dockerPort)),
			ToPort:     aws.Int64(int64(dockerPort)),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(ipRange)}},
		})
	}

	if !hasPorts[fmt.Sprintf("%d/tcp", swarmPort)] && d.SwarmMaster {
		perms = append(perms, &ec2.IpPermission{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(int64(swarmPort)),
			ToPort:     aws.Int64(int64(swarmPort)),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(ipRange)}},
		})
	}

	for _, p := range d.OpenPorts {
		port, protocol := driverutil.SplitPortProto(p)
		portNum, err := strconv.ParseInt(port, 10, 0)
		if err != nil {
			return nil, fmt.Errorf("invalid port number %s: %s", port, err)
		}
		if !hasPorts[fmt.Sprintf("%s/%s", port, protocol)] {
			perms = append(perms, &ec2.IpPermission{
				IpProtocol: aws.String(protocol),
				FromPort:   aws.Int64(portNum),
				ToPort:     aws.Int64(portNum),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(ipRange)}},
			})
		}
	}

	log.Debugf("configuring security group authorization for %s", ipRange)

	return perms, nil
}

func (d *Driver) deleteKeyPair() error {
	if d.KeyName == "" {
		log.Warn("Missing key pair name, this is likely due to a failure during machine creation")
		return nil
	}

	log.Debugf("deleting key pair: %s", d.KeyName)

	_, err := d.getClient().DeleteKeyPair(&ec2.DeleteKeyPairInput{
		KeyName: &d.KeyName,
	})
	if err != nil {
		return err
	}

	return nil
}

func (d *Driver) getDefaultVPCId() (string, error) {
	output, err := d.getClient().DescribeAccountAttributes(&ec2.DescribeAccountAttributesInput{})
	if err != nil {
		return "", err
	}

	for _, attribute := range output.AccountAttributes {
		if *attribute.AttributeName == "default-vpc" {
			value := *attribute.AttributeValues[0].AttributeValue
			if value == "none" {
				return "", errors.New("default-vpc is 'none'")
			}
			return value, nil
		}
	}

	return "", errors.New("No default-vpc attribute")
}

func (d *Driver) getRegionZone() string {
	if d.Endpoint == "" {
		return d.Region + d.Zone
	}
	return d.Zone
}

func generateId() string {
	rb := make([]byte, 10)
	_, err := rand.Read(rb)
	if err != nil {
		log.Warnf("Unable to generate id: %s", err)
	}

	h := md5.New()
	io.WriteString(h, string(rb))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// clamp clamps n to the range [bottom, top]
func clamp[T ~int | ~int8 | ~int16 | ~int32 | ~int64 | ~float32 | ~float64](n, bottom, top T) T {
	switch {
	case n < bottom:
		return bottom
	case n > top:
		return top
	default:
		return n
	}
}
