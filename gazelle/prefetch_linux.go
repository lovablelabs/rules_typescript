package typescript

import (
	"os/exec"
	"syscall"
)

// A cancellable listing must not outlive a Gazelle that exits without cancelling it, as log.Fatal does.
func dieWithParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
