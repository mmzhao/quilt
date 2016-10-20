package command

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"text/scanner"

	log "github.com/Sirupsen/logrus"

	"github.com/NetSys/quilt/api"
	"github.com/NetSys/quilt/stitch"
	"github.com/NetSys/quilt/util"
)

// Run contains the options for running Stitches.
type Run struct {
	stitch string
	host   string

	flags *flag.FlagSet
}

func (rCmd *Run) createFlagSet() *flag.FlagSet {
	flags := flag.NewFlagSet("run", flag.ExitOnError)

	flags.StringVar(&rCmd.stitch, "stitch", "", "the stitch to run")
	flags.StringVar(&rCmd.host, "H", api.DefaultSocket,
		"the host to connect to")

	flags.Usage = func() {
		fmt.Println("usage: quilt run [-H=<daemon_host>] " +
			"[-stitch=<stitch>] <stitch>")
		fmt.Println("`run` compiles the provided stitch, and sends the " +
			"result to the Quilt daemon to be executed.")
		rCmd.flags.PrintDefaults()
	}

	rCmd.flags = flags
	return flags
}

// Parse parses the command line arguments for the run command.
func (rCmd *Run) Parse(args []string) error {
	flags := rCmd.createFlagSet()

	if err := flags.Parse(args); err != nil {
		return err
	}

	if rCmd.stitch == "" {
		nonFlagArgs := flags.Args()
		if len(nonFlagArgs) == 0 {
			return errors.New("no spec specified")
		}
		rCmd.stitch = nonFlagArgs[0]
	}

	return nil
}

// Run starts the run for the provided Stitch.
func (rCmd *Run) Run() int {
	c, err := getClient(rCmd.host)
	if err != nil {
		log.Error(err)
		return 1
	}
	defer c.Close()

	pathStr := stitch.GetQuiltPath()

	spec := rCmd.stitch
	f, err := util.Open(spec)
	if err != nil {
		f, err = util.Open(filepath.Join(pathStr, spec))
		if err != nil {
			log.WithError(err).Errorf("Unable to open %s.", spec)
			return 1
		}
	}

	defer f.Close()

	sc := scanner.Scanner{
		Position: scanner.Position{
			Filename: rCmd.stitch,
		},
	}

	compiled, err := stitch.Compile(*sc.Init(bufio.NewReader(f)),
		stitch.DefaultImportGetter)
	if err != nil {
		log.WithError(err).Errorf("%s failed to compile.", spec)
		return 1
	}

	err = c.RunStitch(compiled)
	if err != nil {
		log.WithError(err).Error("Unable to start run.")
		return 1
	}

	fmt.Println("Successfully started run.")
	return 0
}

// Usage prints the usage for the run command.
func (rCmd *Run) Usage() {
	rCmd.flags.Usage()
}
