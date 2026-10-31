//go:build !unix

package main

import "os/exec"

func detachProcess(cmd *exec.Cmd) {}
