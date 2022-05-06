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
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	K8sDistroARO        = "aro"
	K8sDistroMinikubeGH = "minikube-github"
)

var (
	supportedK8sDistros = []string{K8sDistroARO, K8sDistroMinikubeGH}
	cancelling          = false
)

var (
	integration = flag.Bool("integration", false, "run integration tests")

	// image such as docker.io/kinvolk/gadget:latest
	image = flag.String("image", "", "gadget container image")

	doNotDeployIG  = flag.Bool("no-deploy-ig", false, "don't deploy Inspektor Gadget")
	doNotDeploySPO = flag.Bool("no-deploy-spo", false, "don't deploy the Security Profiles Operator (SPO)")

	k8sDistro = flag.String("k8s-distro", "", "allows to skip tests that are not supported on a given Kubernetes distribution")
)

func runCommands(cmds []*command, t *testing.T) {
	// defer all cleanup commands so we are sure to exit clean whatever
	// happened
	defer func() {
		for _, cmd := range cmds {
			if cmd.cleanup {
				cmd.run(t)
			}
		}
	}()

	// defer stopping commands
	defer func() {
		for _, cmd := range cmds {
			if cmd.startAndStop && cmd.started {
				// Wait a bit before stopping the command.
				time.Sleep(10 * time.Second)
				cmd.stop(t)
			}
		}
	}()

	// run all commands but cleanup ones
	for _, cmd := range cmds {
		if cmd.cleanup {
			continue
		}

		cmd.run(t)
	}
}

func cleanupFunc(cleanupDone chan bool, cleanupCommands []*command) {
	if cancelling {
		// Wait until the other call of cleanupFunc() is done.
		<-cleanupDone
		return
	}

	cancelling = true
	fmt.Println("Cleaning up...")

	// We don't want to wait for each cleanup command to finish before
	// running the next one because in the case the workflow run is
	// cancelled, we have few seconds (7.5s + 2.5s) before the runner kills
	// the entire process tree. Therefore, let's try to, at least, launch
	// the cleanup process in the cluster:
	// https://docs.github.com/en/actions/managing-workflow-runs/canceling-a-workflow#steps-github-takes-to-cancel-a-workflow-run
	for _, cmd := range cleanupCommands {
		err := cmd.startWithoutTest()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
		}
	}

	for _, cmd := range cleanupCommands {
		err := cmd.waitWithoutTest()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
		}
	}

	cleanupDone <- true
}

func testMain(m *testing.M) int {
	flag.Parse()
	if !*integration {
		fmt.Println("Skipping integration test.")
		return 0
	}

	if os.Getenv("KUBECTL_GADGET") == "" {
		fmt.Fprintf(os.Stderr, "please set $KUBECTL_GADGET.")
		return -1
	}

	if *image != "" {
		os.Setenv("GADGET_IMAGE_FLAG", "--image "+*image)
	}

	if *k8sDistro != "" {
		found := false
		for _, val := range supportedK8sDistros {
			if *k8sDistro == val {
				found = true
				break
			}
		}

		if !found {
			fmt.Fprintf(os.Stderr, "Error: invalid argument '-k8s-distro': %q. Valid values: %s\n",
				*k8sDistro, strings.Join(supportedK8sDistros, ", "))
			return -1
		}
	}

	seed := time.Now().UTC().UnixNano()
	rand.Seed(seed)
	fmt.Printf("using random seed: %d\n", seed)

	initCommands := []*command{}
	cleanupCommands := []*command{deleteRemainingNamespacesCommand()}

	if !*doNotDeployIG {
		initCommands = append(initCommands, deployInspektorGadget)
		initCommands = append(initCommands, waitUntilInspektorGadgetPodsDeployed)

		initialDelay := 15
		if *k8sDistro == K8sDistroARO {
			// ARO and any other Kubernetes distribution that uses Red Hat
			// Enterprise Linux CoreOS (RHCOS) requires more time to initialise
			// because we automatically download the kernel headers for it. See
			// gadget-container/entrypoint.sh.
			initialDelay = 60
		}
		initCommands = append(initCommands, waitUntilInspektorGadgetPodsInitialized(initialDelay))

		cleanupCommands = append(cleanupCommands, cleanupInspektorGadget)
	}

	if !*doNotDeploySPO {
		initCommands = append(initCommands, deploySPO)
		cleanupCommands = append(cleanupCommands, cleanupSPO)
	}

	initDone := make(chan bool, 1)
	cleanupDone := make(chan bool, 1)

	cancel := make(chan os.Signal, 1)
	signal.Notify(cancel, syscall.SIGINT)

	go func() {
		handled := false

		for {
			<-cancel
			fmt.Printf("\nHandling cancellation...\n")

			if handled {
				fmt.Println("Warn: Forcing cancellation")
				os.Exit(1)
			}
			handled = true

			go func() {
				// Start by stopping the init commands (in the case they are
				// still running) to avoid trying to undeploy resources that are
				// being deployed.
				fmt.Println("Stop init commands (if they are still running)...")
				for _, cmd := range initCommands {
					err := cmd.killWithoutTest()
					if err != nil {
						fmt.Fprintf(os.Stderr, "%s\n", err)
					}
				}

				// Wait until init commands have exited before starting the
				// cleanup.
				<-initDone

				cleanupFunc(cleanupDone, cleanupCommands)
				os.Exit(-1)
			}()
		}
	}()

	defer cleanupFunc(cleanupDone, cleanupCommands)

	fmt.Println("Running init commands:")

	done := true
	for _, cmd := range initCommands {
		err := cmd.runWithoutTest()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
			done = false
			break
		}
	}

	// Unblock cancelling handler before exiting
	initDone <- done
	if !done {
		return -1
	}

	fmt.Println("Start running tests:")
	return m.Run()
}

func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func TestAuditSeccomp(t *testing.T) {
	if *k8sDistro == K8sDistroARO {
		t.Skip("Skip running audit-seccomp gadget on ARO: see issue #631")
	}

	ns := generateTestNamespaceName("test-audit-seccomp")

	t.Parallel()

	commands := []*command{
		createTestNamespaceCommand(ns),
		{
			name: "CreateSeccompProfile",
			cmd: fmt.Sprintf(`
				kubectl apply -f - <<EOF
apiVersion: security-profiles-operator.x-k8s.io/v1beta1
kind: SeccompProfile
metadata:
  name: log
  namespace: %s
  annotations:
    description: "Log some syscalls"
spec:
  defaultAction: SCMP_ACT_ALLOW
  architectures:
  - SCMP_ARCH_X86_64
  syscalls:
  - action: SCMP_ACT_KILL
    names:
    - unshare
  - action: SCMP_ACT_LOG
    names:
    - mkdir
EOF
			`, ns),
			expectedRegexp: "seccompprofile.security-profiles-operator.x-k8s.io/log created",
		},
		{
			name: "RunSeccompAuditTestPod",
			cmd: fmt.Sprintf(`
				kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-pod
  namespace: %s
spec:
  securityContext:
    seccompProfile:
      type: Localhost
      localhostProfile: operator/%s/log.json
  restartPolicy: Never
  containers:
  - name: container1
    image: busybox
    command: ["sh"]
    args: ["-c", "while true; do unshare -i; sleep 1; done"]
EOF
			`, ns, ns),
			expectedRegexp: "pod/test-pod created",
		},
		waitUntilTestPodReadyCommand(ns),
		{
			name:           "RunAuditSeccompGadget",
			cmd:            fmt.Sprintf("$KUBECTL_GADGET audit seccomp -n %s & sleep 5; kill $!", ns),
			expectedRegexp: fmt.Sprintf(`%s\s+test-pod\s+container1\s+unshare\s+\d+\s+unshare\s+kill_thread`, ns),
		},
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestBindsnoop(t *testing.T) {
	ns := generateTestNamespaceName("test-bindsnoop")

	t.Parallel()

	bindsnoopCmd := &command{
		name:           "StartBindsnoopGadget",
		cmd:            fmt.Sprintf("$KUBECTL_GADGET trace bind -n %s", ns),
		expectedRegexp: fmt.Sprintf(`%s\s+test-pod\s+test-pod\s+\d+\s+nc`, ns),
		startAndStop:   true,
	}

	commands := []*command{
		createTestNamespaceCommand(ns),
		bindsnoopCmd,
		busyboxPodRepeatCommand(ns, "nc -l -p 9090 -w 1"),
		waitUntilTestPodReadyCommand(ns),
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestBiolatency(t *testing.T) {
	t.Parallel()

	commands := []*command{
		{
			name:           "RunBiolatencyGadget",
			cmd:            "id=$($KUBECTL_GADGET profile block-io start --node $(kubectl get node --no-headers | cut -d' ' -f1 | head -1)); sleep 15; $KUBECTL_GADGET profile block-io stop $id",
			expectedRegexp: `usecs\s+:\s+count\s+distribution`,
		},
	}

	runCommands(commands, t)
}

func TestBiotop(t *testing.T) {
	if *k8sDistro == K8sDistroARO {
		t.Skip("Skip running biotop gadget on ARO: see issue #589")
	}

	ns := generateTestNamespaceName("test-biotop")

	t.Parallel()

	biotopCmd := &command{
		name:           "StartBiotopGadget",
		cmd:            fmt.Sprintf("$KUBECTL_GADGET top block-io -n %s", ns),
		expectedRegexp: `test-pod\s+test-pod\s+\d+\s+dd`,
		startAndStop:   true,
	}

	commands := []*command{
		createTestNamespaceCommand(ns),
		biotopCmd,
		busyboxPodRepeatCommand(ns, "dd if=/dev/zero of=/tmp/test count=4096"),
		waitUntilTestPodReadyCommand(ns),
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestCapabilities(t *testing.T) {
	ns := generateTestNamespaceName("test-capabilities")

	t.Parallel()

	capabilitiesCmd := &command{
		name:           "StartCapabilitiesGadget",
		cmd:            fmt.Sprintf("$KUBECTL_GADGET trace capabilities -n %s", ns),
		expectedRegexp: fmt.Sprintf(`%s\s+test-pod.*nice.*CAP_SYS_NICE`, ns),
		startAndStop:   true,
	}

	commands := []*command{
		createTestNamespaceCommand(ns),
		capabilitiesCmd,
		busyboxPodRepeatCommand(ns, "nice -n -20 echo"),
		waitUntilTestPodReadyCommand(ns),
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestDns(t *testing.T) {
	ns := generateTestNamespaceName("test-dns")

	t.Parallel()

	dnsCmd := &command{
		name:           "StartDnsGadget",
		cmd:            fmt.Sprintf("$KUBECTL_GADGET trace dns -n %s", ns),
		expectedRegexp: `test-pod\s+OUTGOING\s+A\s+microsoft.com`,
		startAndStop:   true,
	}

	commands := []*command{
		createTestNamespaceCommand(ns),
		dnsCmd,
		busyboxPodRepeatCommand(ns, "nslookup microsoft.com"),
		waitUntilTestPodReadyCommand(ns),
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestExecsnoop(t *testing.T) {
	ns := generateTestNamespaceName("test-execsnoop")

	t.Parallel()

	execsnoopCmd := &command{
		name:           "StartExecsnoopGadget",
		cmd:            fmt.Sprintf("$KUBECTL_GADGET trace exec -n %s", ns),
		expectedRegexp: fmt.Sprintf(`%s\s+test-pod\s+test-pod\s+date`, ns),
		startAndStop:   true,
	}

	commands := []*command{
		createTestNamespaceCommand(ns),
		execsnoopCmd,
		busyboxPodRepeatCommand(ns, "date"),
		waitUntilTestPodReadyCommand(ns),
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestFiletop(t *testing.T) {
	ns := generateTestNamespaceName("test-filetop")

	t.Parallel()

	filetopCmd := &command{
		name:           "StartFiletopGadget",
		cmd:            fmt.Sprintf("$KUBECTL_GADGET top file -n %s", ns),
		expectedRegexp: fmt.Sprintf(`%s\s+test-pod\s+test-pod\s+\d+\s+\S*\s+0\s+\d+\s+0\s+\d+\s+R\s+date`, ns),
		startAndStop:   true,
	}

	commands := []*command{
		createTestNamespaceCommand(ns),
		filetopCmd,
		busyboxPodRepeatCommand(ns, "echo date >> /tmp/date.txt"),
		waitUntilTestPodReadyCommand(ns),
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestFsslower(t *testing.T) {
	fsType := "ext4"
	if *k8sDistro == K8sDistroARO {
		fsType = "xfs"
	}

	ns := generateTestNamespaceName("test-fsslower")

	t.Parallel()

	fsslowerCmd := &command{
		name:           "StartFsslowerGadget",
		cmd:            fmt.Sprintf("$KUBECTL_GADGET trace fsslower -n %s -t %s -m 0", ns, fsType),
		expectedRegexp: fmt.Sprintf(`%s\s+test-pod\s+test-pod\s+cat`, ns),
		startAndStop:   true,
	}

	commands := []*command{
		createTestNamespaceCommand(ns),
		fsslowerCmd,
		busyboxPodCommand(ns, "echo 'this is foo' > foo && while true; do cat foo && sleep 0.1; done"),
		waitUntilTestPodReadyCommand(ns),
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestMountsnoop(t *testing.T) {
	ns := generateTestNamespaceName("test-mountsnoop")

	t.Parallel()

	mountsnoopCmd := &command{
		name:           "StartMountsnoopGadget",
		cmd:            fmt.Sprintf("$KUBECTL_GADGET trace mount -n %s", ns),
		expectedRegexp: `test-pod\s+test-pod\s+mount.*mount\("/mnt", "/mnt", .*\) = -2`,
		startAndStop:   true,
	}

	commands := []*command{
		createTestNamespaceCommand(ns),
		mountsnoopCmd,
		busyboxPodRepeatCommand(ns, "mount /mnt /mnt"),
		waitUntilTestPodReadyCommand(ns),
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestNetworkpolicy(t *testing.T) {
	ns := generateTestNamespaceName("test-networkpolicy")

	t.Parallel()

	commands := []*command{
		createTestNamespaceCommand(ns),
		busyboxPodRepeatCommand(ns, "wget -q -O /dev/null https://kinvolk.io"),
		waitUntilTestPodReadyCommand(ns),
		{
			name:           "RunNetworkPolicyGadget",
			cmd:            fmt.Sprintf("$KUBECTL_GADGET advise network-policy monitor -n %s --output ./networktrace.log & sleep 15; kill $!; head networktrace.log", ns),
			expectedRegexp: fmt.Sprintf(`"type":"connect".*"%s".*"test-pod"`, ns),
		},
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestOomkill(t *testing.T) {
	ns := generateTestNamespaceName("test-oomkill")

	t.Parallel()

	oomkillCmd := &command{
		name:           "StarOomkilGadget",
		cmd:            fmt.Sprintf("$KUBECTL_GADGET trace oomkill -n %s", ns),
		expectedRegexp: `\d+\s+tail`,
		startAndStop:   true,
	}

	limitPodYaml := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: test-pod
  namespace: %s
spec:
  containers:
  - name: test-pod-container
    image: busybox
    resources:
      limits:
        memory: "128Mi"
    command: ["/bin/sh", "-c"]
    args:
    - while true; do tail /dev/zero; done
`, ns)

	commands := []*command{
		createTestNamespaceCommand(ns),
		oomkillCmd,
		{
			name:           "RunOomkillTestPod",
			cmd:            fmt.Sprintf("echo '%s' | kubectl apply -f -", limitPodYaml),
			expectedRegexp: "pod/test-pod created",
		},
		waitUntilTestPodReadyCommand(ns),
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestOpensnoop(t *testing.T) {
	ns := generateTestNamespaceName("test-opensnoop")

	t.Parallel()

	opensnoopCmd := &command{
		name:           "StartOpensnoopGadget",
		cmd:            fmt.Sprintf("$KUBECTL_GADGET trace open -n %s", ns),
		expectedRegexp: fmt.Sprintf(`%s\s+test-pod\s+test-pod\s+\d+\s+whoami\s+3`, ns),
		startAndStop:   true,
	}

	commands := []*command{
		createTestNamespaceCommand(ns),
		opensnoopCmd,
		busyboxPodRepeatCommand(ns, "whoami"),
		waitUntilTestPodReadyCommand(ns),
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestProcessCollector(t *testing.T) {
	if *k8sDistro == K8sDistroARO {
		t.Skip("Skip running process-collector gadget on ARO: iterators are not supported on kernel 4.18.0-305.19.1.el8_4.x86_64")
	}

	ns := generateTestNamespaceName("test-process-collector")

	t.Parallel()

	commands := []*command{
		createTestNamespaceCommand(ns),
		busyboxPodCommand(ns, "nc -l -p 9090"),
		waitUntilTestPodReadyCommand(ns),
		{
			name:           "RunPprocessCollectorGadget",
			cmd:            fmt.Sprintf("$KUBECTL_GADGET snapshot process -n %s", ns),
			expectedRegexp: fmt.Sprintf(`%s\s+test-pod\s+test-pod\s+nc\s+\d+`, ns),
		},
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestProfile(t *testing.T) {
	ns := generateTestNamespaceName("test-profile")

	t.Parallel()

	commands := []*command{
		createTestNamespaceCommand(ns),
		busyboxPodCommand(ns, "while true; do echo foo > /dev/null; done"),
		waitUntilTestPodReadyCommand(ns),
		{
			name:           "RunProfileGadget",
			cmd:            fmt.Sprintf("$KUBECTL_GADGET profile cpu -n %s -p test-pod -K & sleep 15; kill $!", ns),
			expectedRegexp: `sh;\w+;\w+;\w+open`, // echo is builtin.
		},
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestSeccompadvisor(t *testing.T) {
	ns := generateTestNamespaceName("test-seccomp-advisor")

	t.Parallel()

	commands := []*command{
		createTestNamespaceCommand(ns),
		busyboxPodRepeatCommand(ns, "echo foo"),
		waitUntilTestPodReadyCommand(ns),
		{
			name:           "RunSeccompAdvisorGadget",
			cmd:            fmt.Sprintf("id=$($KUBECTL_GADGET advise seccomp-profile start -n %s -p test-pod); sleep 30; $KUBECTL_GADGET advise seccomp-profile stop $id", ns),
			expectedRegexp: `write`,
		},
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestSigsnoop(t *testing.T) {
	ns := generateTestNamespaceName("test-sigsnoop")

	t.Parallel()

	sigsnoopCmd := &command{
		name:           "StartSigsnoopGadget",
		cmd:            fmt.Sprintf("$KUBECTL_GADGET trace signal -n %s", ns),
		expectedRegexp: fmt.Sprintf(`%s\s+test-pod\s+test-pod\s+\d+\s+sh\s+SIGTERM`, ns),
		startAndStop:   true,
	}

	commands := []*command{
		createTestNamespaceCommand(ns),
		sigsnoopCmd,
		busyboxPodRepeatCommand(ns, "sleep 3 & kill $!"),
		waitUntilTestPodReadyCommand(ns),
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestSnisnoop(t *testing.T) {
	ns := generateTestNamespaceName("test-snisnoop")

	t.Parallel()

	snisnoopCmd := &command{
		name:           "StartSnisnoopGadget",
		cmd:            fmt.Sprintf("$KUBECTL_GADGET trace sni -n %s", ns),
		expectedRegexp: `test-pod\s+kinvolk.io`,
		startAndStop:   true,
	}

	commands := []*command{
		createTestNamespaceCommand(ns),
		snisnoopCmd,
		busyboxPodRepeatCommand(ns, "wget -q -O /dev/null https://kinvolk.io"),
		waitUntilTestPodReadyCommand(ns),
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestSocketCollector(t *testing.T) {
	if *k8sDistro == K8sDistroARO {
		t.Skip("Skip running socket-collector gadget on ARO: iterators are not supported on kernel 4.18.0-305.19.1.el8_4.x86_64")
	}

	ns := generateTestNamespaceName("test-socket-collector")

	t.Parallel()

	commands := []*command{
		createTestNamespaceCommand(ns),
		busyboxPodCommand(ns, "nc -l 0.0.0.0 -p 9090"),
		waitUntilTestPodReadyCommand(ns),
		{
			name:           "RunSocketCollectorGadget",
			cmd:            fmt.Sprintf("$KUBECTL_GADGET snapshot socket -n %s", ns),
			expectedRegexp: fmt.Sprintf(`%s\s+test-pod\s+TCP\s+0\.0\.0\.0`, ns),
		},
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestTcpconnect(t *testing.T) {
	ns := generateTestNamespaceName("test-tcpconnect")

	t.Parallel()

	tcpconnectCmd := &command{
		name:           "StartTcpconnectGadget",
		cmd:            fmt.Sprintf("$KUBECTL_GADGET trace tcpconnect -n %s", ns),
		expectedRegexp: fmt.Sprintf(`%s\s+test-pod\s+test-pod\s+\d+\s+wget`, ns),
		startAndStop:   true,
	}

	commands := []*command{
		createTestNamespaceCommand(ns),
		tcpconnectCmd,
		busyboxPodRepeatCommand(ns, "wget -q -O /dev/null -T 3 http://1.1.1.1"),
		waitUntilTestPodReadyCommand(ns),
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestTcptracer(t *testing.T) {
	ns := generateTestNamespaceName("test-tcptracer")

	t.Parallel()

	tcptracerCmd := &command{
		name:           "StartTcptracerGadget",
		cmd:            fmt.Sprintf("$KUBECTL_GADGET trace tcp -n %s", ns),
		expectedRegexp: `C\s+\d+\s+wget\s+\d\s+[\w\.:]+\s+1\.1\.1\.1\s+\d+\s+80`,
		startAndStop:   true,
	}

	commands := []*command{
		createTestNamespaceCommand(ns),
		tcptracerCmd,
		busyboxPodRepeatCommand(ns, "wget -q -O /dev/null -T 3 http://1.1.1.1"),
		waitUntilTestPodReadyCommand(ns),
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestTcptop(t *testing.T) {
	ns := generateTestNamespaceName("test-tcptop")

	t.Parallel()

	tcptopCmd := &command{
		name:           "StartTcptopGadget",
		cmd:            fmt.Sprintf("$KUBECTL_GADGET top tcp -n %s", ns),
		expectedRegexp: `wget`,
		startAndStop:   true,
	}

	commands := []*command{
		createTestNamespaceCommand(ns),
		tcptopCmd,
		busyboxPodRepeatCommand(ns, "wget -q -O /dev/null https://kinvolk.io"),
		waitUntilTestPodReadyCommand(ns),
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}

func TestTraceloop(t *testing.T) {
	ns := generateTestNamespaceName("test-traceloop")

	t.Parallel()

	commands := []*command{
		createTestNamespaceCommand(ns),
		{
			name: "StartTraceloopGadget",
			cmd:  "$KUBECTL_GADGET traceloop start",
		},
		{
			name: "WaitForTraceloopStarted",
			cmd:  "sleep 15",
		},
		{
			name: "RunTraceloopTestPod",
			cmd:  fmt.Sprintf("kubectl run --restart=Never -n %s --image=busybox multiplication -- sh -c 'RANDOM=output ; echo \"3*7*2\" | bc > /tmp/file-$RANDOM ; sleep infinity'", ns),
		},
		{
			name: "WaitForTraceloopTestPod",
			cmd:  fmt.Sprintf("sleep 5 ; kubectl wait -n %s --for=condition=ready pod/multiplication ; kubectl get pod -n %s ; sleep 2", ns, ns),
		},
		{
			name:           "CheckTraceloopList",
			cmd:            fmt.Sprintf("sleep 20 ; $KUBECTL_GADGET traceloop list -n %s --no-headers | grep multiplication | awk '{print $1\" \"$6}'", ns),
			expectedString: "multiplication started\n",
		},
		{
			name:           "CheckTraceloopShow",
			cmd:            fmt.Sprintf(`TRACE_ID=$($KUBECTL_GADGET traceloop list -n %s --no-headers | `, ns) + `grep multiplication | awk '{printf "%s", $4}') ; $KUBECTL_GADGET traceloop show $TRACE_ID | grep -C 5 write`,
			expectedRegexp: "\\[bc\\] write\\(1, \"42\\\\n\", 3\\)",
		},
		{
			name:    "PrintTraceloopList",
			cmd:     "$KUBECTL_GADGET traceloop list -A",
			cleanup: true,
		},
		{
			name:           "StopTraceloopGadget",
			cmd:            "$KUBECTL_GADGET traceloop stop",
			expectedString: "",
			cleanup:        true,
		},
		{
			name:    "WaitForTraceloopStopped",
			cmd:     "sleep 15",
			cleanup: true,
		},
		deleteTestNamespaceCommand(ns),
	}

	runCommands(commands, t)
}
