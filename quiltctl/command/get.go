package command

import (
	"errors"
	"flag"
	"fmt"

	"github.com/NetSys/quilt/stitch"

	log "github.com/Sirupsen/logrus"
)

// Get contains the options for downloading imports.
type Get struct {
	importPath string

	flags *flag.FlagSet
}

func (gCmd *Get) createFlagSet() *flag.FlagSet {
	flags := flag.NewFlagSet("get", flag.ExitOnError)

	flags.StringVar(&gCmd.importPath, "import", "", "the stitch to download")

	flags.Usage = func() {
		fmt.Println("usage: quilt get [-import=<import>] <import> ")
		fmt.Printf("`get` downloads a given import into %s.\n",
			stitch.QuiltPathKey)
		flags.PrintDefaults()
	}

	gCmd.flags = flags
	return flags
}

// Parse parses the command line arguments for the get command.
func (gCmd *Get) Parse(args []string) error {
	flags := gCmd.createFlagSet()

	if err := flags.Parse(args); err != nil {
		return err
	}

	if gCmd.importPath == "" {
		nonFlagArgs := flags.Args()
		if len(nonFlagArgs) == 0 {
			return errors.New("no import specified")
		}
		gCmd.importPath = nonFlagArgs[0]
	}

	return nil
}

// Run downloads the requested import.
func (gCmd *Get) Run() int {
	if err := stitch.DefaultImportGetter.Get(gCmd.importPath); err != nil {
		log.WithError(err).Errorf("Error getting import `%s`.", gCmd.importPath)
		return 1
	}

	fmt.Println("Successfully installed import.")

	return 0
}

// Usage prints the usage for the get command.
func (gCmd *Get) Usage() {
	gCmd.flags.Usage()
}
