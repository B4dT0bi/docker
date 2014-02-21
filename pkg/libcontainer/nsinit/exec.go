// +build linux

package main

import (
	"fmt"
	"github.com/dotcloud/docker/pkg/libcontainer"
	"github.com/dotcloud/docker/pkg/libcontainer/network"
	"github.com/dotcloud/docker/pkg/libcontainer/utils"
	"github.com/dotcloud/docker/pkg/system"
	"github.com/dotcloud/docker/pkg/term"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"syscall"
)

func execCommand(container *libcontainer.Container, args []string) (int, error) {
	master, console, err := createMasterAndConsole()
	if err != nil {
		return -1, err
	}

	command := createCommand(container, console, args)
	// create a pipe so that we can syncronize with the namespaced process and
	// pass the veth name to the child
	inPipe, err := command.StdinPipe()
	if err != nil {
		return -1, err
	}
	if err := command.Start(); err != nil {
		return -1, err
	}
	if err := writePidFile(command); err != nil {
		command.Process.Kill()
		return -1, err
	}
	defer deletePidFile()

	// Do this before syncing with child so that no children
	// can escape the cgroup
	if container.Cgroups != nil {
		if err := container.Cgroups.Apply(command.Process.Pid); err != nil {
			command.Process.Kill()
			return -1, err
		}
	}

	if container.Network != nil {
		vethPair, err := initializeContainerVeth(container.Network.Bridge, command.Process.Pid)
		if err != nil {
			return -1, err
		}
		sendVethName(vethPair, inPipe)
	}

	// Sync with child
	inPipe.Close()

	go io.Copy(os.Stdout, master)
	go io.Copy(master, os.Stdin)

	state, err := setupWindow(master)
	if err != nil {
		command.Process.Kill()
		return -1, err
	}
	defer term.RestoreTerminal(os.Stdin.Fd(), state)

	if err := command.Wait(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return -1, err
		}
	}
	return command.ProcessState.Sys().(syscall.WaitStatus).ExitStatus(), nil
}

// sendVethName writes the veth pair name to the child's stdin then closes the
// pipe so that the child stops waiting for more data
func sendVethName(name string, pipe io.WriteCloser) {
	fmt.Fprint(pipe, name)
}

// initializeContainerVeth will create a veth pair and setup the host's
// side of the pair by setting the specified bridge as the master and bringing
// up the interface.
//
// Then will with set the other side of the veth pair into the container's namespaced
// using the pid and returns the veth's interface name to provide to the container to
// finish setting up the interface inside the namespace
func initializeContainerVeth(bridge string, nspid int) (string, error) {
	name1, name2, err := createVethPair()
	if err != nil {
		return "", err
	}
	if err := network.SetInterfaceMaster(name1, bridge); err != nil {
		return "", err
	}
	if err := network.InterfaceUp(name1); err != nil {
		return "", err
	}
	if err := network.SetInterfaceInNamespacePid(name2, nspid); err != nil {
		return "", err
	}
	return name2, nil
}

func setupWindow(master *os.File) (*term.State, error) {
	ws, err := term.GetWinsize(os.Stdin.Fd())
	if err != nil {
		return nil, err
	}
	if err := term.SetWinsize(master.Fd(), ws); err != nil {
		return nil, err
	}
	return term.SetRawTerminal(os.Stdin.Fd())
}

// createMasterAndConsole will open /dev/ptmx on the host and retreive the
// pts name for use as the pty slave inside the container
func createMasterAndConsole() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	console, err := system.Ptsname(master)
	if err != nil {
		return nil, "", err
	}
	if err := system.Unlockpt(master); err != nil {
		return nil, "", err
	}
	return master, console, nil
}

// createVethPair will automatically generage two random names for
// the veth pair and ensure that they have been created
func createVethPair() (name1 string, name2 string, err error) {
	name1, err = utils.GenerateRandomName("dock", 4)
	if err != nil {
		return
	}
	name2, err = utils.GenerateRandomName("dock", 4)
	if err != nil {
		return
	}
	if err = network.CreateVethPair(name1, name2); err != nil {
		return
	}
	return
}

// writePidFile writes the namespaced processes pid to .nspid in the rootfs for the container
func writePidFile(command *exec.Cmd) error {
	return ioutil.WriteFile(".nspid", []byte(fmt.Sprint(command.Process.Pid)), 0655)
}

func deletePidFile() error {
	return os.Remove(".nspid")
}

// createCommand will return an exec.Cmd with the Cloneflags set to the proper namespaces
// defined on the container's configuration and use the current binary as the init with the
// args provided
func createCommand(container *libcontainer.Container, console string, args []string) *exec.Cmd {
	command := exec.Command("nsinit", append([]string{"-console", console, "init"}, args...)...)
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: uintptr(getNamespaceFlags(container.Namespaces)),
	}
	return command
}
