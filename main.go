package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"github.com/hanwen/go-fuse/fuse/pathfs"
	"github.com/jessevdk/go-flags"
	"github.com/sevlyar/go-daemon"
)

var (
	// VersionString is the git tag this binary is associated with
	VersionString string
	// RevisionString is the git rev this binary is associated with
	RevisionString string
)

// Options holds the command line arguments and flags
// Intended for use with go-flags
type Options struct {
	Foreground   bool         `short:"f" long:"foreground" description:"Run in foreground"`
	Version      bool         `short:"v" long:"version" description:"Display version info"`
	MountOptions mountOptions `short:"o" long:"options" description:"These options will be passed through to FUSE. Please see the OPTIONS section of the FUSE manual for valid options"`

	Args struct {
		Endpoint   string `positional-arg-name:"endpoint" description:"Endpoint of the EC2 metadata service, set to 'default' to use http://169.254.169.254/latest/meta-data/"`
		Mountpoint string `positional-arg-name:"mountpoint" description:"Directory to mount the filesystem"`
	} `positional-args:"yes" required:"yes"`
}

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

func mountAndServe(endpoint, mountpoint string, opts mountOptions) {
	nfs := pathfs.NewPathNodeFs(NewMetadataFs(endpoint), nil)
	server, err := fuse.NewServer(nodefs.NewFileSystemConnector(nfs.Root(), nil).RawFS(), mountpoint, &fuse.MountOptions{Options: opts.opts})
	if err != nil {
		log.Fatalf("mount fail: %v\n", err)
	}
	server.Serve()
}

func main() {
	options := &Options{}

	parser := flags.NewParser(options, flags.Default)
	parser.LongDescription = `
ec2metadafs mounts a FUSE filesystem at the given location which exposes the
EC2 instance metadata of the host as files and directories mimicking the URL
structure of the metadata service.`

	_, err := parser.Parse()
	if err != nil {
		if err.(*flags.Error).Type == flags.ErrHelp {
			fmt.Printf(`Version:
  %s (%s)

Author:
  Jesse Szwedko

Project Homepage:
  http://github.com/jszwedko/ec2-metadatafs

Report bugs to:
  http://github.com/jszwedko/ec2-metadatafs/issues
`, VersionString, RevisionString)
			os.Exit(0)
		} else {
			os.Exit(1)
		}
	}

	if options.Version {
		fmt.Printf("%s (%s)\n", VersionString, RevisionString)
		os.Exit(0)
	}

	if options.Args.Endpoint == "default" {
		options.Args.Endpoint = "http://169.254.169.254/latest/"
	}

	if options.Foreground {
		mountAndServe(options.Args.Endpoint, options.Args.Mountpoint, options.MountOptions)
		return
	}

	// daemonize
	context := new(daemon.Context)
	child, err := context.Reborn()
	if err != nil {
		log.Fatalf("mount fail: %v\n", err)
	}

	if child == nil {
		defer context.Release()
		mountAndServe(options.Args.Endpoint, options.Args.Mountpoint, options.MountOptions)
	}
}
