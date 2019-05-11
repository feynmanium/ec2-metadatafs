package main

import (
	"fmt"
	"log"
	"log/syslog"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"github.com/hanwen/go-fuse/fuse/pathfs"
	"github.com/hanwen/go-fuse/unionfs"
	"github.com/jessevdk/go-flags"
	"github.com/jszwedko/ec2-metadatafs/internal/logging"
	"github.com/jszwedko/ec2-metadatafs/metadatafs"
	"github.com/jszwedko/ec2-metadatafs/tagsfs"
	"github.com/sevlyar/go-daemon"
)

const (
	verbose     = 1
	moreVerbose = 2
)

var (
	// VersionString is the git tag this binary is associated with
	VersionString string

	facilityMap = map[string]syslog.Priority{
		"KERN":     syslog.LOG_KERN,
		"USER":     syslog.LOG_USER,
		"MAIL":     syslog.LOG_MAIL,
		"DAEMON":   syslog.LOG_DAEMON,
		"AUTH":     syslog.LOG_AUTH,
		"SYSLOG":   syslog.LOG_SYSLOG,
		"LPR":      syslog.LOG_LPR,
		"NEWS":     syslog.LOG_NEWS,
		"UUCP":     syslog.LOG_UUCP,
		"CRON":     syslog.LOG_CRON,
		"AUTHPRIV": syslog.LOG_AUTHPRIV,
		"FTP":      syslog.LOG_FTP,

		"LOCAL0": syslog.LOG_LOCAL0,
		"LOCAL1": syslog.LOG_LOCAL1,
		"LOCAL2": syslog.LOG_LOCAL2,
		"LOCAL3": syslog.LOG_LOCAL3,
		"LOCAL4": syslog.LOG_LOCAL4,
		"LOCAL5": syslog.LOG_LOCAL5,
		"LOCAL6": syslog.LOG_LOCAL6,
		"LOCAL7": syslog.LOG_LOCAL7,
	}
)

func sortedValidFacilities() []string {
	facilities := FacilityNamesSlice(make([]string, 0, len(facilityMap)))
	for k := range facilityMap {
		facilities = append(facilities, k)
	}
	sort.Sort(facilities)
	return facilities
}

// FacilityNamesSlice supports sorting of facility names
type FacilityNamesSlice []string

func (f FacilityNamesSlice) Len() int {
	return len(f)
}

func (f FacilityNamesSlice) Less(i, j int) bool {
	return facilityMap[f[i]] < facilityMap[f[j]]
}

func (f FacilityNamesSlice) Swap(i, j int) {
	f[i], f[j] = f[j], f[i]
}

// Options holds the command line arguments and flags
// Intended for use with go-flags
type Options struct {
	Verbose      []bool       `short:"v" long:"verbose"     description:"Print verbose logs, can be specified multiple times (up to 2)"`
	Foreground   bool         `short:"f" long:"foreground"  description:"Run in foreground"`
	Version      bool         `short:"V" long:"version"     description:"Display version info"`
	Endpoint     string       `short:"e" long:"endpoint"    description:"EC2 metadata service HTTP endpoint" default:"http://169.254.169.254/latest/"`
	CacheSec     int          `short:"c" long:"cachesec"    description:"Number of seconds to cache files attributes and directory listings. 0 to disable, -1 for indefinite." default:"0"`
	Tags         bool         `short:"t" long:"tags"        description:"Mount EC2 instance tags at <mount point>/tags"`
	MountOptions mountOptions `short:"o" long:"options"     description:"Mount options, see below for description"`

	DisableSyslog  bool   `short:"n" long:"no-syslog"        description:"Disable syslog when daemonized"`
	SyslogFacility string `short:"F" long:"syslog-facility"  description:"Syslog facility to use when daemonized (see below for options)" default:"USER"`

	AWSCredentials awsCredentials `group:"AWS Credentials (only used when mounting tags)"`

	Args struct {
		Mountpoint string `positional-arg-name:"mountpoint"   description:"Directory to mount the filesystem at"`
	} `positional-args:"yes" required:"yes"`
}

type awsCredentials struct {
	AWSAccessKeyID     string `long:"aws-access-key-id"     description:"AWS Access Key ID (adds to credential chain, see below)"`
	AWSSecretAccessKey string `long:"aws-secret-access-key" description:"AWS Secret Access key (adds to credential chain, see below)"`
	AWSSessionToken    string `long:"aws-session-token"     description:"AWS session token (adds to credential chain, see below)"`
}

func (a *awsCredentials) credentialChain() *credentials.Credentials {
	return credentials.NewChainCredentials([]credentials.Provider{
		&credentials.StaticProvider{Value: credentials.Value{
			AccessKeyID:     a.AWSAccessKeyID,
			SecretAccessKey: a.AWSAccessKeyID,
			SessionToken:    a.AWSSessionToken}},
		&credentials.EnvProvider{},
		&credentials.SharedCredentialsProvider{},
		&ec2rolecreds.EC2RoleProvider{Client: ec2metadata.New(session.New())},
	})
}

// mountOptions implements flags.Marshaller and flags.Unmarshaller interface to
// read `mount` style options from the user
type mountOptions struct {
	opts []string
}

func (o *mountOptions) String() string {
	return strings.Join(o.opts, ",")
}

func (o *mountOptions) MarshalFlag() (string, error) {
	return o.String(), nil
}

func (o *mountOptions) UnmarshalFlag(s string) error {
	if o.opts == nil {
		o.opts = []string{}
	}

	o.opts = append(o.opts, strings.Split(s, ",")...)

	return nil
}

// ExtractOption deletes the option specified and returns whether the option
// was found and its value (if it has one)
// E.g. endpoint=http://example.com or allow_other
func (o *mountOptions) ExtractOption(s string) (ok bool, value string) {
	if o.opts == nil {
		o.opts = []string{}
	}

	index := -1
	for i, opt := range o.opts {
		parts := strings.SplitN(opt, "=", 2)

		if parts[0] != s {
			continue
		}

		index = i
		if len(parts) == 2 {
			value = parts[1]
		}
		break
	}

	if index != -1 {
		o.opts = append(o.opts[:index], o.opts[index+1:]...)
	}

	return index != -1, value
}

// mountTags mounts another endpoint onto the FUSE FS at tags/ exposing the EC2
// instance tags as files
func mountTags(nfs *pathfs.PathNodeFs, options *Options, logger *logging.Logger) {
	svc := ec2metadata.New(session.New())
	instanceID, err := svc.GetMetadata("instance-id")
	if err != nil {
		logger.Fatalf("failed to query instance id to initialize tags mount: %v\n", err)
	}
	region, err := svc.Region()
	if err != nil {
		logger.Fatalf("failed to query instance region to initialize tags mount: %v\n", err)
	}

	sess := session.New(&aws.Config{
		Region:      aws.String(region),
		Credentials: options.AWSCredentials.credentialChain(),
	})

	status := nfs.Mount(
		"tags",
		pathfs.NewPathNodeFs(tagsfs.New(ec2.New(sess), instanceID, logger), nil).Root(), nil)
	if status != fuse.OK {
		logger.Fatalf("tags mount fail: %v\n", status)
	}
}

func prepareServer(options *Options, logger *logging.Logger) *fuse.Server {
	var fs pathfs.FileSystem

	logger.Debugf("mounting at %s directed at %s with options: %+v", options.Args.Mountpoint, options.Endpoint, options.MountOptions.opts)
	fs = metadatafs.New(options.Endpoint, logger)
	switch {
	case options.CacheSec == 0:
		logger.Debugf("caching disabled")
	case options.CacheSec <= 0:
		logger.Debugf("indefinite caching enabled")
		fs = unionfs.NewCachingFileSystem(fs, time.Duration(-1)*time.Second)
	default:
		logger.Debugf("caching enabled (%d seconds)", options.CacheSec)
		fs = unionfs.NewCachingFileSystem(fs, time.Duration(options.CacheSec)*time.Second)
	}

	nfs := pathfs.NewPathNodeFs(fs, nil)
	server, err := fuse.NewServer(
		nodefs.NewFileSystemConnector(nfs.Root(), nil).RawFS(),
		options.Args.Mountpoint,
		&fuse.MountOptions{Options: options.MountOptions.opts})
	if err != nil {
		logger.Fatalf("mount fail: %v\n", err)
	}

	server.SetDebug(len(options.Verbose) >= moreVerbose)

	if options.Tags {
		go func() {
			server.WaitMount()
			logger.Debugf("mounting tags")
			mountTags(nfs, options, logger)
			logger.Debugf("tags mounted")
		}()
	}

	// Unmount when the process exits
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		err := server.Unmount()
		if err != nil {
			logger.Warningf("could not unmount: %s", err)
		}
		os.Exit(1)
	}()

	return server
}

// signal the parent of our process that we started successfully so it can exit
func sigalParent(logger *logging.Logger) {
	pid, err := strconv.Atoi(os.Getenv("EC2_METADATAFS_NOTIFY"))
	if err != nil {
		logger.Warningf("unable to decode parent pid for notification: %s", err)
		return
	}

	p, err := os.FindProcess(pid)
	if err != nil {
		logger.Warningf("unable to find parent pid for notification: %s", err)
		return
	}

	err = p.Signal(syscall.SIGUSR1)
	if err != nil {
		logger.Warningf("unable to find notify parent: %s", err)
	}
}

func waitForSignal(logger *logging.Logger) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGUSR1)

	select {
	case <-c:
		logger.Infof("child process successfully mounted")
	case <-time.After(time.Second * 5):
		logger.Fatalf("timeout waiting for child process to mount, try running in the foreground")
	}
}

func main() {
	options := &Options{}

	logger := logging.NewLogger()
	defer logger.Close()

	// go-fuse logging uses the stdlib logger
	debugWriter := logger.Writer(logging.DebugLevel)
	defer debugWriter.Close()
	log.SetOutput(debugWriter)
	log.SetFlags(0)

	parser := flags.NewParser(options, flags.HelpFlag|flags.PassDoubleDash)
	parser.LongDescription = `
ec2metadatafs mounts a FUSE filesystem which exposes the EC2 instance metadata
(and optionally the tags) of the host as files and directories rooted at the
given location.`

	_, err := parser.Parse()
	if err, ok := err.(*flags.Error); err != nil && (!ok || err.Type != flags.ErrHelp) {
		logger.Fatalf("error parsing command line options: %s", err)
	}

	if options.Version {
		fmt.Printf("%s\n", VersionString)
		os.Exit(0)
	}

	if parser.FindOptionByLongName("help").IsSet() {
		parser.WriteHelp(os.Stdout)
		fmt.Printf(`
Mount options:
  -o debug                     Enable debug logging, same as -v
  -o fuse_debug                Enable fuse_debug logging (implies debug), same as -vv
  -o endpoint=ENDPOINT         EC2 metadata service HTTP endpoint, same as --endpoint=
  -o tags                      Mount the instance tags at <mount point>/tags, same as --tags
  -o aws_access_key_id=ID      AWS API access key (see below), same as --aws-access-key-id=
  -o aws_secret_access_key=KEY AWS API secret key (see below), same as --aws-secret-access-key=
  -o aws_session_token=KEY     AWS API session token (see below), same as --aws-session-token=
  -o cachesec=SEC              Number of seconds to cache files attributes and directory listings, same as --cachesec
  -o syslog_facility=					 Syslog facility to send messages upon when daemonized (see below)
  -o no_syslog                 Disable logging to syslog when daemonized
  -o FUSEOPTION=OPTIONVALUE    FUSE mount option, please see the OPTIONS section of your FUSE manual for valid options

AWS credential chain:
  AWS credentials only required when mounting the instance tags (--tags or -o tags).

  Checks for credentials in the following places, in order:

  - Provided AWS credentials via flags or mount options
  - $AWS_ACCESS_KEY_ID, $AWS_SECRET_ACCESS_KEY, and $AWS_SESSION_TOKEN environment variables
  - Shared credentials file -- respects $AWS_DEFAULT_PROFILE and $AWS_SHARED_CREDENTIALS_FILE
  - IAM role associated with the instance

  Note that the AWS session token is only needed for temporary credentials from AWS security token service.

Caching:

Caching of the following is supported and controlled via the cachesec parameter:

* File attributes
* Directory attributes
* Directory listings

When accessed this metadata will be cached for the number of seconds specified
by cachesec. Use 0, the default, to disable caching and -1 to cache
indefinitely (good if you never expect instance metadata to change). This cache
is kept in memory and lost when the process is restarted.

Valid syslog facilities:
  %s

Version:
  %s

Author:
  Jesse Szwedko

Project Homepage:
  http://github.com/jszwedko/ec2-metadatafs

Report bugs to:
  http://github.com/jszwedko/ec2-metadatafs/issues
`, strings.Join(sortedValidFacilities(), ", "), VersionString)
		os.Exit(0)
	}

	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if ok, value := options.MountOptions.ExtractOption("endpoint"); ok {
		options.Endpoint = value
	}

	if ok, value := options.MountOptions.ExtractOption("aws_access_key_id"); ok {
		options.AWSCredentials.AWSAccessKeyID = value
	}

	if ok, value := options.MountOptions.ExtractOption("aws_secret_access_key"); ok {
		options.AWSCredentials.AWSSecretAccessKey = value
	}

	if ok, value := options.MountOptions.ExtractOption("aws_session_token"); ok {
		options.AWSCredentials.AWSSessionToken = value
	}

	if ok, value := options.MountOptions.ExtractOption("cachesec"); ok {
		options.CacheSec, err = strconv.Atoi(value)
		if err != nil {
			fmt.Printf("error parsing cachesec as integer: %s\n", err)
			os.Exit(1)
		}
	}

	if ok, _ := options.MountOptions.ExtractOption("tags"); ok {
		options.Tags = true
	}

	if ok, _ := options.MountOptions.ExtractOption("no_syslog"); ok {
		options.DisableSyslog = true
	}

	if ok, value := options.MountOptions.ExtractOption("syslog_facility"); ok {
		options.SyslogFacility = value
	}

	if ok, _ := options.MountOptions.ExtractOption("debug"); ok {
		options.Verbose = []bool{true}
	}

	if ok, _ := options.MountOptions.ExtractOption("fuse_debug"); ok {
		options.Verbose = []bool{true, true}
	}

	syslogFacility, ok := facilityMap[options.SyslogFacility]
	if !ok {
		logger.Fatalf("unrecognized syslog facility: %s", options.SyslogFacility)
	}

	if len(options.Verbose) >= verbose {
		logger.MinLevel = logging.DebugLevel
	}

	if options.Foreground {
		prepareServer(options, logger).Serve()
		return
	}

	// daemonize
	if !options.DisableSyslog {
		logger.EnableSyslog(syslogFacility)
	}

	context := &daemon.Context{Env: append(os.Environ(), fmt.Sprintf("EC2_METADATAFS_NOTIFY=%d", os.Getpid()))}

	child, err := context.Reborn()
	if err != nil {
		logger.Fatalf("fork fail: %s", err)
	}

	if child == nil {
		defer context.Release()

		server := prepareServer(options, logger)
		go func() {
			server.WaitMount()
			sigalParent(logger)
		}()
		server.Serve()
	} else {
		logger.Infof("forked child with PID %d", child.Pid)
		waitForSignal(logger)
	}
}
