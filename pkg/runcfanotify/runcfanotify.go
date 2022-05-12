// Copyright 2021 The Inspektor Gadget authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runcfanotify

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	ocispec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/s3rj1k/go-fanotify/fanotify"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

type EventType int

const (
	EventTypeAddContainer EventType = iota
	EventTypeRemoveContainer
)

// ContainerEvent is the notification for container creation or termination
type ContainerEvent struct {
	// Type is whether the container was added or removed
	Type EventType

	// ContainerID is the container id, typically a 64 hexadecimal string
	ContainerID string

	// ContainerPID is the process id of the container
	ContainerPID uint32

	// Container's configuration is the config.json from the OCI runtime
	// spec
	ContainerConfig *ocispec.Spec
}

type RuncNotifyFunc func(notif ContainerEvent)

type RuncNotifier struct {
	runcBinaryNotify *fanotify.NotifyFD
	callback         RuncNotifyFunc

	// containers is the set of containers that are being watched for
	// termination. This prevents duplicate calls to
	// AddWatchContainerTermination.
	//
	// Keys: Container ID
	// Value: dummy struct
	containers map[string]struct{}
	mu         sync.Mutex

	// set to true when RuncNotifier is closed
	closed bool
}

// runcPaths is the list of paths where runc could be installed. Depending on
// the Linux distribution, it could be in different locations.
//
// When this package is executed in a container, it looks at the /host volume.
var runcPaths = []string{
	"/usr/bin/runc",
	"/usr/sbin/runc",
	"/usr/local/sbin/runc",
	"/run/torcx/unpack/docker/bin/runc",

	"/host/usr/bin/runc",
	"/host/usr/sbin/runc",
	"/host/usr/local/sbin/runc",
	"/host/run/torcx/unpack/docker/bin/runc",
}

// initFanotify initializes the fanotify API with the flags we need
func initFanotify() (*fanotify.NotifyFD, error) {
	fanotifyFlags := uint(unix.FAN_CLOEXEC | unix.FAN_CLASS_CONTENT | unix.FAN_UNLIMITED_QUEUE | unix.FAN_UNLIMITED_MARKS | unix.FAN_NONBLOCK)
	openFlags := os.O_RDONLY | unix.O_LARGEFILE | unix.O_CLOEXEC
	return fanotify.Initialize(fanotifyFlags, openFlags)
}

// Supported detects if RuncNotifier is supported in the current environment
func Supported() bool {
	// Test that runc is available
	runcFound := false
	for _, path := range runcPaths {
		if _, err := os.Stat(path); err == nil {
			runcFound = true
			break
		}
	}
	if !runcFound {
		return false
	}
	// Test that it's possible to run fanotify
	notifyFD, err := initFanotify()
	if err != nil {
		return false
	}
	notifyFD.File.Close()
	return true
}

// NewRuncNotifier uses fanotify to detect when runc containers are created
// or terminated, and call the callback on such event.
//
// Limitations:
// - runc must be installed in one of the paths listed by runcPaths
func NewRuncNotifier(callback RuncNotifyFunc) (*RuncNotifier, error) {
	n := &RuncNotifier{
		callback:   callback,
		containers: make(map[string]struct{}),
	}

	runcBinaryNotify, err := initFanotify()
	if err != nil {
		return nil, err
	}
	n.runcBinaryNotify = runcBinaryNotify

	for _, file := range runcPaths {
		err = runcBinaryNotify.Mark(unix.FAN_MARK_ADD, unix.FAN_OPEN_EXEC_PERM, unix.AT_FDCWD, file)
		if err == nil {
			log.Debugf("Checking %q: done", file)
		} else {
			log.Debugf("Checking %q: %s", file, err)
		}
	}

	go n.watchRunc()

	return n, nil
}

func commFromPid(pid int) string {
	comm, _ := ioutil.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	return strings.TrimSuffix(string(comm), "\n")
}

func cmdlineFromPid(pid int) []string {
	cmdline, _ := ioutil.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return strings.Split(string(cmdline), "\x00")
}

// AddWatchContainerTermination watches a container for termination and
// generates an event on the notifier. This is automatically called for new
// containers detected by RuncNotifier, but it can also be called for
// containers detected externally such as initial containers.
func (n *RuncNotifier) AddWatchContainerTermination(containerID string, containerPID int) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if _, ok := n.containers[containerID]; ok {
		// This container is already being watched for termination
		return nil
	}
	n.containers[containerID] = struct{}{}

	pidfd, _, errno := unix.Syscall(unix.SYS_PIDFD_OPEN, uintptr(containerPID), 0, 0)
	if errno == unix.ENOSYS {
		// pidfd_open not available. As a fallback, check if the
		// process exists every second
		go n.watchContainerTerminationFallback(containerID, containerPID)
		return nil
	}
	if errno != 0 {
		return fmt.Errorf("pidfd_open returned %w", errno)
	}

	// watch for container termination with pidfd_open
	go n.watchContainerTermination(containerID, containerPID, int(pidfd))
	return nil
}

// watchContainerTermination waits until the container terminates using
// pidfd_open (Linux >= 5.3), then sends a notification.
func (n *RuncNotifier) watchContainerTermination(containerID string, containerPID int, pidfd int) {
	defer func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		delete(n.containers, containerID)
	}()

	defer unix.Close(pidfd)

	for {
		if n.closed {
			return
		}
		fds := []unix.PollFd{
			{
				Fd:      int32(pidfd),
				Events:  unix.POLLIN,
				Revents: 0,
			},
		}
		count, err := unix.Poll(fds, 1000)
		if err == nil && count == 1 {
			n.callback(ContainerEvent{
				Type:         EventTypeRemoveContainer,
				ContainerID:  containerID,
				ContainerPID: uint32(containerPID),
			})
			return
		}
	}
}

// watchContainerTerminationFallback waits until the container terminates
// *without* using pidfd_open so it works on older kernels, then sends a notification.
func (n *RuncNotifier) watchContainerTerminationFallback(containerID string, containerPID int) {
	defer func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		delete(n.containers, containerID)
	}()

	for {
		if n.closed {
			return
		}
		time.Sleep(time.Second)
		process, err := os.FindProcess(containerPID)
		if err == nil {
			// no signal is sent: signal 0 just check for the
			// existence of the process
			err = process.Signal(syscall.Signal(0))
		}

		if err != nil {
			n.callback(ContainerEvent{
				Type:         EventTypeRemoveContainer,
				ContainerID:  containerID,
				ContainerPID: uint32(containerPID),
			})
			return
		}
	}
}

func (n *RuncNotifier) watchPidFileIterate(pidFileDirNotify *fanotify.NotifyFD, bundleDir string, pidFile string, pidFileDir string) (bool, error) {
	// Get the next event from fanotify.
	// Even though the API allows to pass skipPIDs, we cannot use
	// it here because ResponseAllow would not be called.
	data, err := pidFileDirNotify.GetEvent()
	if err != nil {
		return false, fmt.Errorf("%w", err)
	}

	// data can be nil if the event received is from a process in skipPIDs.
	// In that case, skip and get the next event.
	if data == nil {
		return false, nil
	}

	// Don't leak the fd received by GetEvent
	defer data.Close()
	dataFile := data.File()
	defer dataFile.Close()

	if !data.MatchMask(unix.FAN_ACCESS_PERM) {
		// This should not happen: FAN_ACCESS_PERM is the only mask Marked
		return false, fmt.Errorf("fanotify: unknown event on runc: mask=%d pid=%d", data.Mask, data.Pid)
	}

	// This unblocks whoever is accessing the pidfile
	defer pidFileDirNotify.ResponseAllow(data)

	pid := data.GetPID()

	// Skip events triggered by ourselves
	if pid == os.Getpid() {
		return false, nil
	}

	path, err := data.GetPath()
	if err != nil {
		return false, err
	}
	if path != pidFile {
		return false, nil
	}

	pidFileContent, err := ioutil.ReadAll(dataFile)
	if err != nil {
		return false, err
	}
	if len(pidFileContent) == 0 {
		return false, fmt.Errorf("empty pid file")
	}
	containerPID, err := strconv.Atoi(string(pidFileContent))
	if err != nil {
		return false, err
	}

	// Unfortunately, Linux 5.4 doesn't respect ignore masks
	// See fix in Linux 5.9:
	// https://github.com/torvalds/linux/commit/497b0c5a7c0688c1b100a9c2e267337f677c198e
	// Workaround: remove parent mask. We don't need it anymore :)
	err = pidFileDirNotify.Mark(unix.FAN_MARK_REMOVE, unix.FAN_ACCESS_PERM|unix.FAN_EVENT_ON_CHILD, unix.AT_FDCWD, pidFileDir)
	if err != nil {
		return false, nil
	}

	bundleConfigJSON, err := ioutil.ReadFile(filepath.Join(bundleDir, "config.json"))
	if err != nil {
		return false, err
	}
	containerConfig := &ocispec.Spec{}
	err = json.Unmarshal(bundleConfigJSON, containerConfig)
	if err != nil {
		return false, err
	}

	containerID := filepath.Base(filepath.Clean(bundleDir))

	err = n.AddWatchContainerTermination(containerID, containerPID)
	if err != nil {
		log.Errorf("runc fanotify: container %s with pid %d terminated before we could watch it: %s", containerID, containerPID, err)
		return true, nil
	}

	n.callback(ContainerEvent{
		Type:            EventTypeAddContainer,
		ContainerID:     containerID,
		ContainerPID:    uint32(containerPID),
		ContainerConfig: containerConfig,
	})
	return true, nil
}

func (n *RuncNotifier) monitorRuncInstance(bundleDir string, pidFile string) error {
	fanotifyFlags := uint(unix.FAN_CLOEXEC | unix.FAN_CLASS_CONTENT | unix.FAN_UNLIMITED_QUEUE | unix.FAN_UNLIMITED_MARKS)
	openFlags := os.O_RDONLY | unix.O_LARGEFILE | unix.O_CLOEXEC

	pidFileDirNotify, err := fanotify.Initialize(fanotifyFlags, openFlags)
	if err != nil {
		return err
	}

	// The pidfile does not exist yet, so we cannot monitor it directly.
	// Instead we monitor its parent directory with FAN_EVENT_ON_CHILD to
	// get events on the directory's children.
	pidFileDir := filepath.Dir(pidFile)
	err = pidFileDirNotify.Mark(unix.FAN_MARK_ADD, unix.FAN_ACCESS_PERM|unix.FAN_EVENT_ON_CHILD, unix.AT_FDCWD, pidFileDir)
	if err != nil {
		pidFileDirNotify.File.Close()
		return fmt.Errorf("cannot mark %s: %w", bundleDir, err)
	}

	// watchPidFileIterate() will read config.json and it might be in the
	// same directory as the pid file. To avoid getting events unrelated to
	// the pidfile, add an ignore mask.
	//
	// This is best effort because the ignore mask is unfortunately not
	// respected until a fix in Linux 5.9:
	// https://github.com/torvalds/linux/commit/497b0c5a7c0688c1b100a9c2e267337f677c198e
	configJSONPath := filepath.Join(bundleDir, "config.json")
	err = pidFileDirNotify.Mark(unix.FAN_MARK_ADD|unix.FAN_MARK_IGNORED_MASK, unix.FAN_ACCESS_PERM, unix.AT_FDCWD, configJSONPath)
	if err != nil {
		pidFileDirNotify.File.Close()
		return fmt.Errorf("cannot ignore %s: %w", configJSONPath, err)
	}

	go func() {
		for {
			stop, err := n.watchPidFileIterate(pidFileDirNotify, bundleDir, pidFile, pidFileDir)
			if n.closed {
				pidFileDirNotify.File.Close()
				return
			}
			if err != nil {
				log.Errorf("error watching pid: %v\n", err)
			}
			if stop {
				pidFileDirNotify.File.Close()
				return
			}
		}
	}()

	return nil
}

func (n *RuncNotifier) watchRunc() {
	for {
		stop, err := n.watchRuncIterate()
		if n.closed {
			n.runcBinaryNotify.File.Close()
			return
		}
		if err != nil {
			log.Errorf("error watching runc: %v\n", err)
		}
		if stop {
			n.runcBinaryNotify.File.Close()
			return
		}
	}
}

func (n *RuncNotifier) watchRuncIterate() (bool, error) {
	// Get the next event from fanotify.
	// Even though the API allows to pass skipPIDs, we cannot use it here
	// because ResponseAllow would not be called.
	data, err := n.runcBinaryNotify.GetEvent()
	if err != nil {
		return true, fmt.Errorf("%w", err)
	}

	// data can be nil if the event received is from a process in skipPIDs.
	// In that case, skip and get the next event.
	if data == nil {
		return false, nil
	}

	// Don't leak the fd received by GetEvent
	defer data.Close()

	if !data.MatchMask(unix.FAN_OPEN_EXEC_PERM) {
		// This should not happen: FAN_OPEN_EXEC_PERM is the only mask Marked
		return false, fmt.Errorf("fanotify: unknown event on runc: mask=%d pid=%d", data.Mask, data.Pid)
	}

	// This unblocks the execution
	defer n.runcBinaryNotify.ResponseAllow(data)

	pid := data.GetPID()

	// Skip events triggered by ourselves
	if pid == os.Getpid() {
		return false, nil
	}

	// runc is executing itself with unix.Exec(), so fanotify receives two
	// FAN_OPEN_EXEC_PERM events:
	//   1. from containerd-shim (or similar)
	//   2. from runc, by this re-execution.
	// This filter skips the first one and handles the second one.
	if commFromPid(pid) != "runc" {
		return false, nil
	}

	// Parse runc command line
	cmdlineArr := cmdlineFromPid(pid)
	createFound := false
	bundleDir := ""
	pidFile := ""
	for i := 0; i < len(cmdlineArr); i++ {
		if cmdlineArr[i] == "create" {
			createFound = true
			continue
		}
		if cmdlineArr[i] == "--bundle" && i+1 < len(cmdlineArr) {
			i++
			bundleDir = cmdlineArr[i]
			continue
		}
		if cmdlineArr[i] == "--pid-file" && i+1 < len(cmdlineArr) {
			i++
			pidFile = cmdlineArr[i]
			continue
		}
	}

	if createFound && bundleDir != "" && pidFile != "" {
		err := n.monitorRuncInstance(bundleDir, pidFile)
		if err != nil {
			log.Errorf("error monitoring runc instance: %v\n", err)
		}
	}

	return false, nil
}

func (n *RuncNotifier) Close() {
	n.closed = true
	n.runcBinaryNotify.File.Close()

	// do not return until all go routines are done
	for len(n.containers) != 0 {
		time.Sleep(100 * time.Millisecond)
	}
}
