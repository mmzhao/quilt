package command

import (
	"fmt"
)

// DaemonConnectError represents when we are unable to connect to the Quilt daemon.
type DaemonConnectError struct {
	host         string
	connectError error
}

func (err DaemonConnectError) Error() string {
	return fmt.Sprintf("Unable to connect to the Quilt daemon at %s: %s. "+
		"Is the quilt daemon running? If not, you can start it with "+
		"`quilt daemon`.", err.host, err.connectError.Error())
}
