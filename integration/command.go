// Copyright 2019-2021 The Inspektor Gadget authors
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

package main

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/kr/pretty"
)

const (
	namespaceLabelKey   string = "scope"
	namespaceLabelValue string = "ig-integration-tests"
)

type command struct {
	// name of the command to be run, used to give information.
	name string

	// cmd is a string of the command which will be run.
	cmd string

	// command is a Cmd object used when we want to start the command, then other
	// do stuff and wait for its completion.
	command *exec.Cmd

	// stdout contains command standard output when started using Startcommand().
	stdout bytes.Buffer

	// stderr contains command standard output when started using Startcommand().
	stderr bytes.Buffer

	// expectedString contains the exact expected output of the command.
	expectedString string

	// expectedRegexp contains a regex used to match against the command output.
	expectedRegexp string

	// cleanup indicates this command is used to clean resource and should not be
	// skipped even if previous commands failed.
	cleanup bool

	// startAndStop indicates this command should first be started then stopped.
	// It corresponds to gadget like execsnoop which wait user to type Ctrl^C.
	startAndStop bool

	// started indicates this command was started.
	// It is only used by command which have startAndStop set.
	started bool
}

var deployInspektorGadget *command = &command{
	name:           "Deploy Inspektor Gadget",
	cmd:            "$KUBECTL_GADGET deploy $GADGET_IMAGE_FLAG | kubectl apply -f -",
	expectedRegexp: "gadget created",
}

var waitUntilInspektorGadgetPodsDeployed *command = &command{
	name: "Wait until the gadget pods are started",
	cmd: `
	for POD in $(sleep 5; kubectl get pod -n gadget -l k8s-app=gadget -o name) ; do
		kubectl wait --timeout=30s -n gadget --for=condition=ready $POD
		if [ $? -ne 0 ]; then
			kubectl get pod -n gadget
			kubectl describe $POD -n gadget
			exit 1
		fi
	done`,
}

var deploySPO *command = &command{
	name: "Deploy Security Profiles Operator (SPO)",
	// The security-profiles-operator-webhook deployment is not part of the
	// yaml but created by SPO. We cannot use kubectl-wait before it is
	// created, see also:
	// https://github.com/kubernetes/kubernetes/issues/83242
	// Unfortunately, it takes quite a while for
	// security-profiles-operator-webhook to be started, hence the long
	// timeout
	cmd: `
	kubectl apply -f https://github.com/jetstack/cert-manager/releases/download/v1.7.1/cert-manager.yaml
	kubectl --namespace cert-manager wait --for condition=ready pod -l app.kubernetes.io/instance=cert-manager
	curl https://raw.githubusercontent.com/kubernetes-sigs/security-profiles-operator/main/deploy/operator.yaml | \
		sed 's/replicas: 3/replicas: 1/'|grep -v cpu: | \
		kubectl apply -f -
	for i in $(seq 1 120); do
		if [ "$(kubectl get pod -n security-profiles-operator -l app=security-profiles-operator,name=security-profiles-operator-webhook -o go-template='{{len .items}}')" -ge 1 ] ; then
			break
		fi
		sleep 1
	done
	kubectl patch deploy -n security-profiles-operator security-profiles-operator-webhook --type=json \
		-p='[{"op": "remove", "path": "/spec/template/spec/containers/0/resources"}]'
	kubectl patch ds -n security-profiles-operator spod --type=json \
		-p='[{"op": "remove", "path": "/spec/template/spec/containers/0/resources"}, {"op": "remove", "path": "/spec/template/spec/containers/1/resources"}, {"op": "remove", "path": "/spec/template/spec/initContainers/0/resources"}]'
	kubectl --namespace security-profiles-operator wait --for condition=ready pod -l app=security-profiles-operator || (kubectl get pod -n security-profiles-operator ; kubectl get events -n security-profiles-operator ; false)
	`,
	expectedRegexp: "pod/security-profiles-operator-.*-.* condition met",
}

func waitUntilInspektorGadgetPodsInitialized(initialDelay int) *command {
	return &command{
		name: "Wait until Inspektor Gadget is initialised",
		cmd:  fmt.Sprintf("sleep %d", initialDelay),
	}
}

var cleanupInspektorGadget *command = &command{
	name:    "cleanup gadget deployment",
	cmd:     "$KUBECTL_GADGET undeploy",
	cleanup: true,
}

var cleanupSPO *command = &command{
	name: "Remove Security Profiles Operator (SPO)",
	cmd: `
	kubectl delete seccompprofile -n security-profiles-operator --all
	kubectl delete -f https://raw.githubusercontent.com/kubernetes-sigs/security-profiles-operator/main/deploy/operator.yaml
	kubectl delete -f https://github.com/jetstack/cert-manager/releases/download/v1.7.1/cert-manager.yaml
	`,
	cleanup: true,
}

// createExecCmd creates an exec.Cmd for the command c.cmd and stores it in
// command.command. The exec.Cmd is configured to store the stdout and stderr in
// command.stdout and command.stderr so that we can use them on
// command.verifyOutput().
func (c *command) createExecCmd() {
	cmd := exec.Command("/bin/sh", "-c", c.cmd)

	cmd.Stdout = &c.stdout
	cmd.Stderr = &c.stderr

	// To be able to kill the process of /bin/sh and its child (the process of
	// c.cmd), we need to send the termination signal to their process group ID
	// (PGID). However, child processes get the same PGID as their parents by
	// default, so in order to avoid killing also the integration tests process,
	// we set the fields Setpgid and Pgid of syscall.SysProcAttr before
	// executing /bin/sh. Doing so, the PGID of /bin/sh (and its children)
	// will be set to its process ID, see:
	// https://cs.opensource.google/go/go/+/refs/tags/go1.17.8:src/syscall/exec_linux.go;l=32-34.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: 0}

	c.command = cmd
}

// getInspektorGadgetLogs returns a string with the logs of the gadget pods
func getInspektorGadgetLogs() string {
	var sb strings.Builder

	logCommands := []string{
		"kubectl get pods -n gadget -o wide",
		`for pod in $(kubectl get pods -n gadget -o name); do
			kubectl logs -n gadget $pod;
		done`,
	}

	for _, c := range logCommands {
		cmd := exec.Command("/bin/sh", "-xc", c)
		output, err := cmd.CombinedOutput()
		if err != nil {
			sb.WriteString(fmt.Sprintf("Error: failed to run log command: %s\n", cmd.String()))
			continue
		}
		sb.WriteString(string(output))
	}

	return sb.String()
}

// verifyOutput verifies if the stdout match with the expected regular
// expression and the expected string. If it doesn't, verifyOutput returns and
// error and the gadget pod logs.
func (c *command) verifyOutput() error {
	output := c.stdout.String()

	if c.expectedRegexp != "" {
		r := regexp.MustCompile(c.expectedRegexp)
		if !r.MatchString(output) {
			return fmt.Errorf("output didn't match the expected regexp: %s\n%s",
				c.expectedRegexp, getInspektorGadgetLogs())
		}
	}

	if c.expectedString != "" && output != c.expectedString {
		return fmt.Errorf("output didn't match the expected string: %s\n%v\n%s",
			c.expectedString, pretty.Diff(c.expectedString, output), getInspektorGadgetLogs())
	}

	return nil
}

// kill kills a command by sending SIGKILL because we want to stop the process
// immediatly and avoid that the signal is trapped.
func kill(cmd *exec.Cmd, wait bool) error {
	const sig syscall.Signal = syscall.SIGKILL

	// No need to kill, command has not been executed yet or it already exited
	if cmd == nil || (cmd.ProcessState != nil && cmd.ProcessState.Exited()) {
		return nil
	}

	// Given that we set Setpgid, here we just need to send the PID of /bin/sh
	// (which is the same PGID) as a negative number to syscall.Kill(). As a
	// result, the signal will be received by all the processes with such PGID,
	// in our case, the process of /bin/sh and c.cmd.
	err := syscall.Kill(-cmd.Process.Pid, sig)
	if err != nil {
		return err
	}

	// In some cases, we do not have to wait here because the cmd was executed
	// with Run(), which already waits. On the contrary, in the case it was
	// executed with Start(), we need to wait indeed.
	if wait {
		err = cmd.Wait()
		if err == nil {
			return nil
		}

		// Verify if the error is about the signal we just sent. In that case,
		// do not return error, it is what we were expecting.
		var exiterr *exec.ExitError
		if ok := errors.As(err, &exiterr); !ok {
			return err
		}

		waitStatus, ok := exiterr.Sys().(syscall.WaitStatus)
		if !ok {
			return err
		}

		if waitStatus.Signal() != sig {
			return err
		}

		return nil
	}

	return err
}

// runWithoutTest runs the command, this is thought to be used in TestMain().
func (c *command) runWithoutTest() error {
	c.createExecCmd()

	fmt.Printf("Run command: %s\n", c.cmd)
	err := c.command.Run()
	fmt.Printf("Command returned:\n%s\n%s\n", c.stderr.String(), c.stdout.String())

	if err != nil {
		return err
	}

	return c.verifyOutput()
}

// startWithoutTest starts the command, this is thought to be used in TestMain().
func (c *command) startWithoutTest() error {
	if c.started {
		fmt.Printf("Warn: trying to start a command but it is not running: %s\n", c.cmd)
		return nil
	}

	c.createExecCmd()

	fmt.Printf("Start command: %s\n", c.cmd)
	err := c.command.Start()
	if err != nil {
		return err
	}

	c.started = true

	return nil
}

// waitWithoutTest waits for a command that was started with startWithoutTest(),
// this is thought to be used in TestMain().
func (c *command) waitWithoutTest() error {
	if !c.started {
		fmt.Printf("Warn: trying to wait for a command that has not been started yet: %s\n", c.cmd)
		return nil
	}

	fmt.Printf("Wait for command: %s\n", c.cmd)
	err := c.command.Wait()
	fmt.Printf("Command returned:\n%s\n%s\n", c.stderr.String(), c.stdout.String())

	if err != nil {
		return err
	}

	c.started = false

	return nil
}

// killWithoutTest kills for a command that was started with startWithoutTest()
// or runWithoutTest() and we do not need to verify its output. This is thought
// to be used in TestMain().
func (c *command) killWithoutTest() error {
	fmt.Printf("Kill command: %s\n", c.cmd)
	return kill(c.command, c.started)
}

// run runs the command on the given as parameter test.
func (c *command) run(t *testing.T) {
	c.createExecCmd()

	if c.startAndStop {
		c.start(t)
		return
	}

	t.Logf("Run command: %s\n", c.cmd)
	err := c.command.Run()
	t.Logf("Command returned:\n%s\n%s\n", c.stderr.String(), c.stdout.String())

	if err != nil {
		t.Fatal(err)
	}

	err = c.verifyOutput()
	if err != nil {
		t.Fatal(err)
	}
}

// start starts the command on the given as parameter test, you need to
// wait it using stop().
func (c *command) start(t *testing.T) {
	if c.started {
		t.Logf("Warn: trying to start command but it was already started: %s\n", c.cmd)
		return
	}

	t.Logf("Start command: %s\n", c.cmd)
	err := c.command.Start()
	if err != nil {
		t.Fatal(err)
	}

	c.started = true
}

// stop stops a command previously started with start().
// To do so, it Kill() the process corresponding to this Cmd and then wait for
// its termination.
// Cmd output is then checked with regard to expectedString and expectedRegexp
func (c *command) stop(t *testing.T) {
	if !c.started {
		t.Logf("Warn: trying to stop command but it was not started: %s\n", c.cmd)
		return
	}

	t.Logf("Stop command: %s\n", c.cmd)
	err := kill(c.command, c.started)
	t.Logf("Command returned:\n%s\n%s\n", c.stderr.String(), c.stdout.String())

	if err != nil {
		t.Fatal(err)
	}

	err = c.verifyOutput()
	if err != nil {
		t.Fatal(err)
	}

	c.started = false
}

// busyboxPodRepeatCommand returns a command that creates a pod and runs
// "cmd" each 0.1 seconds inside the pod.
func busyboxPodRepeatCommand(namespace, cmd string) *command {
	cmdStr := fmt.Sprintf("while true; do %s && sleep 0.1; done", cmd)
	return busyboxPodCommand(namespace, cmdStr)
}

// busyboxPodCommand returns a command that creates a pod and runs "cmd" in it.
func busyboxPodCommand(namespace, cmd string) *command {
	cmdStr := fmt.Sprintf(`kubectl apply -f - <<"EOF"
apiVersion: v1
kind: Pod
metadata:
  name: test-pod
  namespace: %s
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 0
  containers:
  - name: test-pod
    image: busybox
    command: ["/bin/sh", "-c"]
    args:
    - %s
EOF
`, namespace, cmd)

	return &command{
		name:           "Run test-pod",
		cmd:            cmdStr,
		expectedString: "pod/test-pod created\n",
	}
}

// generateTestNamespaceName returns a string which can be used as unique
// namespace.
// The returned value is: namespace_parameter-random_integer.
func generateTestNamespaceName(namespace string) string {
	return fmt.Sprintf("%s-%d", namespace, rand.Int())
}

// createTestNamespaceCommand returns a command which creates a namespace whom
// name is given as parameter.
func createTestNamespaceCommand(namespace string) *command {
	return &command{
		name: "Create test namespace",
		cmd: fmt.Sprintf(`kubectl create namespace %s --dry-run=client -o yaml | \
			sed  '/^metadata:/a\ \ labels: {"%s":"%s"}' | kubectl apply -f - `,
			namespace, namespaceLabelKey, namespaceLabelValue),
		expectedString: fmt.Sprintf("namespace/%s created\n", namespace),
	}
}

// deleteTestNamespaceCommand returns a command which deletes a namespace whom
// name is given as parameter.
func deleteTestNamespaceCommand(namespace string) *command {
	return &command{
		name:           "Delete test namespace",
		cmd:            fmt.Sprintf("kubectl delete ns %s", namespace),
		expectedString: fmt.Sprintf("namespace \"%s\" deleted\n", namespace),
		cleanup:        true,
	}
}

// deleteRemainingNamespacesCommand returns a command which deletes a namespace whom
// name is given as parameter.
func deleteRemainingNamespacesCommand() *command {
	return &command{
		name: "Delete remaining test namespace",
		cmd: fmt.Sprintf("kubectl delete ns -l %s=%s",
			namespaceLabelKey, namespaceLabelValue),
		cleanup: true,
	}
}

// waitUntilTestPodReadyCommand returns a command which waits until test-pod in
// the given as parameter namespace is ready.
func waitUntilTestPodReadyCommand(namespace string) *command {
	return &command{
		name:           "Wait until test pod ready",
		cmd:            fmt.Sprintf("kubectl wait pod --for condition=ready -n %s test-pod", namespace),
		expectedString: "pod/test-pod condition met\n",
	}
}
