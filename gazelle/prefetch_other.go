//go:build !linux

package typescript

import "os/exec"

func dieWithParent(*exec.Cmd) {}
