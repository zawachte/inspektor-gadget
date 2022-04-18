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

package utils

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"google.golang.org/grpc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/transport/spdy"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/portforward"
	watchtools "k8s.io/client-go/tools/watch"

	gadgetv1alpha1 "github.com/kinvolk/inspektor-gadget/pkg/apis/gadget/v1alpha1"
	clientset "github.com/kinvolk/inspektor-gadget/pkg/client/clientset/versioned"
	"github.com/kinvolk/inspektor-gadget/pkg/k8sutil"

	pb "github.com/kinvolk/inspektor-gadget/pkg/gadgettracermanager/api"
)

const (
	GadgetOperation = "gadget.kinvolk.io/operation"
	// We name it "global" as if one trace is created on several nodes, then each
	// copy of the trace on each node will share the same id.
	GlobalTraceID = "global-trace-id"
	TraceTimeout  = 5 * time.Second
)

// TraceConfig is used to contain information used to manage a trace.
type TraceConfig struct {
	// GadgetName is gadget name, e.g. socket-collector.
	GadgetName string

	// Operation is the gadget operation to apply to this trace, e.g. start to
	// start the tracing.
	Operation string

	// TraceOutputMode is the trace output mode, the correct values are:
	// * "Status": The trace prints information when its status changes.
	// * "Stream": The trace prints information as events arrive.
	// * "File": The trace prints information into a file.
	// * "ExternalResource": The trace prints information an external resource,
	// e.g. a seccomp profile.
	TraceOutputMode string

	// TraceOutputState is the state in which the trace can output information.
	// For example, trace for *-collector gadget contains output while in
	// Completed state.
	// But other gadgets, like dns, can contain output only in Started state.
	TraceOutputState string

	// TraceOutput is either the name of the file when TraceOutputMode is File or
	// the name of the external resource when TraceOutputMode is ExternalResource.
	// Otherwise, its value is ignored.
	TraceOutput string

	// TraceInitialState is the state in which the trace should be after its
	// creation.
	// This field is only used by "multi-rounds gadgets" like biolatency.
	TraceInitialState string

	// CommonFlags is used to hold parameters given on the command line interface.
	CommonFlags *CommonFlags

	// Parameters is used to pass specific gadget configurations.
	Parameters map[string]string
}

func init() {
	// The Trace REST client needs to know the Trace CRD
	gadgetv1alpha1.AddToScheme(scheme.Scheme)

	// useful for randomTraceID()
	rand.Seed(time.Now().UnixNano())
}

func randomTraceID() string {
	output := make([]byte, 16)
	allowedCharacters := "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	for i := range output {
		output[i] = allowedCharacters[rand.Int31n(int32(len(allowedCharacters)))]
	}
	return string(output)
}

// If all the elements in the map have the same value, it is returned.
// Otherwise, an empty string is returned.
func getIdenticalValue(m map[string]string) string {
	value := ""
	for _, v := range m {
		if value == "" {
			value = v
		} else if value != v {
			return ""
		}
	}
	return value
}

// If there are more than one element in the map and the Error/Warning is
// the same for all the nodes, printTraceFeedback will print it only once.
func printTraceFeedback(prefix string, m map[string]string, totalNodes int) {
	// Do not print `len(m)` times the same message if it's the same from all nodes
	if len(m) > 1 && len(m) == totalNodes {
		value := getIdenticalValue(m)
		if value != "" {
			fmt.Fprintf(os.Stderr, "%s: %s\n",
				prefix, WrapInErrRunGadgetOnAllNode(errors.New(value)))
			return
		}
	}

	for node, msg := range m {
		fmt.Fprintf(os.Stderr, "%s: %s\n",
			prefix, WrapInErrRunGadgetOnNode(node, errors.New(msg)))
	}
}

func deleteTraces(traceClient *clientset.Clientset, traceID string) {
	listTracesOptions := metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", GlobalTraceID, traceID),
	}

	err := traceClient.GadgetV1alpha1().Traces("gadget").DeleteCollection(
		context.TODO(), metav1.DeleteOptions{}, listTracesOptions,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error deleting traces: %q", err)
	}
}

func GetTraceClient() (*clientset.Clientset, error) {
	return getTraceClient()
}

func getTraceClient() (*clientset.Clientset, error) {
	config, err := KubernetesConfigFlags.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to creating RESTConfig: %w", err)
	}

	traceClient, err := clientset.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to set up trace client: %w", err)
	}

	return traceClient, err
}

// createTraces creates a trace using Kubernetes REST API.
// Note that, this function will create the trace on all existing node if
// trace.Spec.Node is empty.
func createTraces(trace *gadgetv1alpha1.Trace) error {
	client, err := k8sutil.NewClientsetFromConfigFlags(KubernetesConfigFlags)
	if err != nil {
		return WrapInErrSetupK8sClient(err)
	}

	traceClient, err := getTraceClient()
	if err != nil {
		return err
	}

	nodes, err := client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return WrapInErrListNodes(err)
	}

	traceNode := trace.Spec.Node
	for _, node := range nodes.Items {
		if traceNode != "" && node.Name != traceNode {
			continue
		}
		// If no particular node was given, we need to apply this trace on all
		// available nodes.
		if traceNode == "" {
			trace.Spec.Node = node.Name
		}

		_, err := traceClient.GadgetV1alpha1().Traces("gadget").Create(
			context.TODO(), trace, metav1.CreateOptions{},
		)
		if err != nil {
			traceID, present := trace.ObjectMeta.Labels[GlobalTraceID]
			if present {
				// Clean before exiting!
				deleteTraces(traceClient, traceID)
			}

			return fmt.Errorf("failed to create trace on node %q: %w", node.Name, err)
		}
	}

	return nil
}

// updateTraceOperation updates operation for an already existing trace using
// Kubernetes REST API.
func updateTraceOperation(trace *gadgetv1alpha1.Trace, operation string) error {
	traceClient, err := getTraceClient()
	if err != nil {
		return err
	}

	// This trace will be used as JSON merge patch to update GADGET_OPERATION,
	// see:
	// https://datatracker.ietf.org/doc/html/rfc6902
	// https://datatracker.ietf.org/doc/html/rfc7386
	type Annotations map[string]string
	type ObjectMeta struct {
		Annotations Annotations `json:"annotations"`
	}
	type JSONMergePatch struct {
		ObjectMeta ObjectMeta `json:"metadata"`
	}
	patch := JSONMergePatch{
		ObjectMeta: ObjectMeta{
			Annotations{
				GadgetOperation: operation,
			},
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal the operation annotations: %w", err)
	}

	_, err = traceClient.GadgetV1alpha1().Traces("gadget").Patch(
		context.TODO(), trace.ObjectMeta.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{},
	)

	return err
}

// CreateTrace initializes a trace object with its field according to the given
// parameter.
// The trace is then posted to the RESTClient which returns an error if
// something wrong occurred.
// A unique trace identifier is returned, this identifier will be used as other
// function parameter.
// A trace obtained with this function must be deleted calling DeleteTrace.
// Note that, if config.TraceInitialState is not empty, this function will
// succeed only if the trace was created and goes into the requested state.
func CreateTrace(config *TraceConfig) (string, error) {
	traceID := randomTraceID()

	var filter *gadgetv1alpha1.ContainerFilter

	// Keep Filter field empty if it is not really used
	if config.CommonFlags.Namespace != "" || config.CommonFlags.Podname != "" ||
		config.CommonFlags.Containername != "" || len(config.CommonFlags.Labels) > 0 {
		filter = &gadgetv1alpha1.ContainerFilter{
			Namespace:     config.CommonFlags.Namespace,
			Podname:       config.CommonFlags.Podname,
			ContainerName: config.CommonFlags.Containername,
			Labels:        config.CommonFlags.Labels,
		}
	}

	trace := &gadgetv1alpha1.Trace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: config.GadgetName + "-",
			Namespace:    "gadget",
			Annotations: map[string]string{
				GadgetOperation: config.Operation,
			},
			Labels: map[string]string{
				GlobalTraceID: traceID,
				// Add all this information here to be able to find the trace thanks
				// to them when calling getTraceListFromParameters().
				"gadgetName":    config.GadgetName,
				"nodeName":      config.CommonFlags.Node,
				"namespace":     config.CommonFlags.Namespace,
				"podName":       config.CommonFlags.Podname,
				"containerName": config.CommonFlags.Containername,
				"outputMode":    config.TraceOutputMode,
				// We will not add config.TraceOutput as label because it can contain
				// "/" which is forbidden in labels.
			},
		},
		Spec: gadgetv1alpha1.TraceSpec{
			Node:       config.CommonFlags.Node,
			Gadget:     config.GadgetName,
			Filter:     filter,
			RunMode:    "Manual",
			OutputMode: config.TraceOutputMode,
			Output:     config.TraceOutput,
			Parameters: config.Parameters,
		},
	}

	err := createTraces(trace)
	if err != nil {
		return "", err
	}

	if config.TraceInitialState != "" {
		// Once the traces are created, we wait for them to be in
		// config.TraceInitialState state, so they are ready to be used by the user.
		_, err = waitForTraceState(traceID, config.TraceInitialState)
		if err != nil {
			deleteError := DeleteTrace(traceID)

			if deleteError != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
			}

			return "", err
		}
	}

	return traceID, nil
}

// getTraceListFromOptions returns a list of traces corresponding to the given
// options.
func getTraceListFromOptions(listTracesOptions metav1.ListOptions) (*gadgetv1alpha1.TraceList, error) {
	traceClient, err := getTraceClient()
	if err != nil {
		return nil, err
	}

	return traceClient.GadgetV1alpha1().Traces("gadget").List(
		context.TODO(), listTracesOptions,
	)
}

// getTraceListFromID returns an array of pointers to gadgetv1alpha1.Trace
// corresponding to the given traceID.
// If no trace corresponds to this ID, error is set.
func getTraceListFromID(traceID string) (*gadgetv1alpha1.TraceList, error) {
	listTracesOptions := metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", GlobalTraceID, traceID),
	}

	traces, err := getTraceListFromOptions(listTracesOptions)
	if err != nil {
		return traces, fmt.Errorf("failed to get traces from traceID %q: %w", traceID, err)
	}

	if len(traces.Items) == 0 {
		return traces, fmt.Errorf("no traces found for traceID %q", traceID)
	}

	return traces, nil
}

// SetTraceOperation sets the operation of an existing trace.
// If trace does not exist an error is returned.
func SetTraceOperation(traceID string, operation string) error {
	// We have to wait for the previous operation to start before changing the
	// trace operation.
	// The trace controller deletes the GADGET_OPERATION field from Annotations
	// when it is about to deal with an operation.
	// Thus, to avoid losing operations, we need to wait for GADGET_OPERATION to
	// be deleted before changing to the current operation.
	// It is the same like when you are in the restaurant, you need to wait for
	// the chef to cook the main dishes before ordering the dessert.
	traces, err := waitForNoOperation(traceID)
	if err != nil {
		return err
	}

	for _, trace := range traces.Items {
		localError := updateTraceOperation(&trace, operation)
		if localError != nil {
			err = fmt.Errorf("%w\nError updating trace operation for %q: %s", err, traceID, localError)
		}
	}

	return err
}

// untilWithoutRetry is a simplified version (only one function as argument)
// version of UntilWithoutRetry, we keep this here because UntilWithoutRetry
// could be deprecated in the future.
// As archive, here is UntilWithoutRetry documentation:
// UntilWithoutRetry reads items from the watch until each provided condition succeeds, and then returns the last watch
// encountered. The first condition that returns an error terminates the watch (and the event is also returned).
// If no event has been received, the returned event will be nil.
// Conditions are satisfied sequentially so as to provide a useful primitive for higher level composition.
// Waits until context deadline or until context is canceled.
//
// Warning: Unless you have a very specific use case (probably a special Watcher) don't use this function!!!
// Warning: This will fail e.g. on API timeouts and/or 'too old resource version' error.
// Warning: You are most probably looking for a function *Until* or *UntilWithSync* below,
// Warning: solving such issues.
// TODO: Consider making this function private to prevent misuse when the other occurrences in our codebase are gone.
func untilWithoutRetry(ctx context.Context, watcher watch.Interface, condition func(event watch.Event) (bool, error)) (*watch.Event, error) {
	ch := watcher.ResultChan()
	defer watcher.Stop()

	var retEvent *watch.Event

Loop:
	for {
		select {
		case event, ok := <-ch:
			if !ok {
				return retEvent, errors.New("watch closed before untilWithoutRetry timeout")
			}
			retEvent = &event

			done, err := condition(event)
			if err != nil {
				return retEvent, err
			}
			if done {
				break Loop
			}

		case <-ctx.Done():
			return retEvent, wait.ErrWaitTimeout
		}
	}

	return retEvent, nil
}

// getTraceWatcher returns a watcher on trace(s) for the received ID.
// If resourceVersion is set, the watcher will watch for traces which have at
// least the received ResourceVersion, otherwise it will watch all traces.
// This watcher can then be used to wait until the State.Output is modified.
func getTraceWatcher(traceID, resourceVersion string) (watch.Interface, error) {
	traceClient, err := getTraceClient()
	if err != nil {
		return nil, err
	}

	watchOptions := metav1.ListOptions{
		LabelSelector:   fmt.Sprintf("%s=%s", GlobalTraceID, traceID),
		ResourceVersion: resourceVersion,
	}

	watcher, err := traceClient.GadgetV1alpha1().Traces("gadget").Watch(context.TODO(), watchOptions)
	if err != nil {
		return nil, err
	}

	return watcher, nil
}

// waitForCondition waits for the traces with the ID received as parameter to
// satisfy the conditionFunction received as parameter.
func waitForCondition(traceID string, conditionFunction func(*gadgetv1alpha1.Trace) bool) (*gadgetv1alpha1.TraceList, error) {
	satisfiedTraces := make(map[string]*gadgetv1alpha1.Trace)
	erroredTraces := make(map[string]*gadgetv1alpha1.Trace)
	var returnedTraces gadgetv1alpha1.TraceList
	nodeWarnings := make(map[string]string)
	nodeErrors := make(map[string]string)

	traceList, err := getTraceListFromID(traceID)
	if err != nil {
		return nil, err
	}

	// Maybe some traces already satisfy conditionFunction?
	for i, trace := range traceList.Items {
		if trace.Status.OperationWarning != "" {
			// The trace can have a warning but satisfies conditionFunction.
			// So, we do not add it to the map here.
			nodeWarnings[trace.Spec.Node] = trace.Status.OperationWarning
		}

		if trace.Status.OperationError != "" {
			erroredTraces[trace.ObjectMeta.Name] = &traceList.Items[i]

			continue
		}

		if !conditionFunction(&trace) {
			continue
		}

		satisfiedTraces[trace.ObjectMeta.Name] = &traceList.Items[i]
	}

	tracesNumber := len(traceList.Items)

	// We only watch the traces if there are some which did not already satisfy
	// the conditionFunction.
	if len(satisfiedTraces)+len(erroredTraces) < tracesNumber {
		var watcher watch.Interface

		// We will need to watch events on them.
		// For this, we will get a watcher on all the traces which share the same
		// ID.
		// Get a watcher on all the traces which have the same ID.
		// Indeed, all the traces on different nodes but linked to one gadget share
		// the same ID.
		// We will also begin to monitor events since the above GET of the traces
		// list.
		watcher, err = getTraceWatcher(traceID, traceList.ListMeta.ResourceVersion)
		if err != nil {
			return nil, err
		}

		ctx, cancel := watchtools.ContextWithOptionalTimeout(context.Background(), TraceTimeout)
		_, err = untilWithoutRetry(ctx, watcher, func(event watch.Event) (bool, error) {
			// This function will be executed until:
			// 1. The number of watched traces equals the number of traces to watch,
			// i.e. we dealt with the traces which interest us.
			// 2. Or it returns an error.
			// 3. Or time out is fired.
			// NOTE In case 2 and 3, it exists, at least, one trace we did not deal
			// with.
			switch event.Type {
			case watch.Deleted:
				// If for some strange reasons (e.g. users deleted a trace during this
				// operation) a trace is deleted, we need to take care of this by
				// decrementing the tracesNumber.
				// Otherwise we would still wait for the old number and we would
				// timeout.
				tracesNumber--

				trace, _ := event.Object.(*gadgetv1alpha1.Trace)
				traceName := trace.ObjectMeta.Name

				// We also remove it from the maps to avoid returning a deleted trace
				// and timeing out.
				delete(satisfiedTraces, traceName)
				delete(erroredTraces, traceName)

				return false, nil
			case watch.Modified:
				// We will deal with this type of event below
			case watch.Error:
				// Deal particularly with error.
				return false, fmt.Errorf("received event is an error one: %v", event)
			case watch.Added:
				// createTraces() creates traces synchronously.
				// So, if a watch.Added event occurs it means there is a problem (e.g.
				// the user creates a trace by snooping on the traceID of existing
				// traces).
				return false, fmt.Errorf("no traces with the given traceID (%s) should be created", traceID)
			default:
				// We are not interested in other event types.
				return false, nil
			}

			trace, _ := event.Object.(*gadgetv1alpha1.Trace)

			if trace.Status.OperationWarning != "" {
				// The trace can have a warning but satisfies conditionFunction.
				// So, we do not add it to the map here.
				nodeWarnings[trace.Spec.Node] = trace.Status.OperationWarning
			}

			if trace.Status.OperationError != "" {
				erroredTraces[trace.ObjectMeta.Name] = trace

				// If the trace satisfied the function, we do not care now because it
				// has an error.
				delete(satisfiedTraces, trace.ObjectMeta.Name)

				return len(satisfiedTraces)+len(erroredTraces) == tracesNumber, nil
			}

			// If the trace does not satisfy the condition function, we are not
			// interested.
			if !conditionFunction(trace) {
				return false, nil
			}

			satisfiedTraces[trace.ObjectMeta.Name] = trace

			return len(satisfiedTraces)+len(erroredTraces) == tracesNumber, nil
		})
		cancel()
	}

	for _, trace := range erroredTraces {
		nodeErrors[trace.Spec.Node] = trace.Status.OperationError
	}

	// We print errors whatever happened.
	printTraceFeedback("Error", nodeErrors, tracesNumber)

	// We print warnings only if all trace failed.
	if len(satisfiedTraces) == 0 {
		printTraceFeedback("Warn", nodeWarnings, tracesNumber)
	}

	if err != nil {
		return nil, err
	}

	for _, trace := range satisfiedTraces {
		returnedTraces.Items = append(returnedTraces.Items, *trace)
	}

	return &returnedTraces, nil
}

// waitForTraceState waits for the traces with the ID received as parameter to
// be in the expected state.
func waitForTraceState(traceID string, expectedState string) (*gadgetv1alpha1.TraceList, error) {
	return waitForCondition(traceID, func(trace *gadgetv1alpha1.Trace) bool {
		return trace.Status.State == expectedState
	})
}

// waitForNoOperation waits for the traces with the ID received as parameter to
// not have an operation.
func waitForNoOperation(traceID string) (*gadgetv1alpha1.TraceList, error) {
	return waitForCondition(traceID, func(trace *gadgetv1alpha1.Trace) bool {
		if trace.ObjectMeta.Annotations == nil {
			return true
		}

		_, present := trace.ObjectMeta.Annotations[GadgetOperation]
		return !present
	})
}

var sigIntReceivedNumber = 0

// sigHandler installs a handler for all signals which cause termination as
// their default behavior.
// On reception of this signal, the given trace will be deleted.
// This function fixes trace not being deleted when calling:
// kubectl gadget process-collector -A | head -n0
func sigHandler(traceID *string) {
	c := make(chan os.Signal)
	signal.Notify(c, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGILL, syscall.SIGABRT, syscall.SIGFPE, syscall.SIGKILL, syscall.SIGSEGV, syscall.SIGPIPE, syscall.SIGALRM, syscall.SIGTERM, syscall.SIGBUS, syscall.SIGTRAP)
	go func() {
		sig := <-c

		// This code is here in case DeleteTrace() hangs.
		// In this case, we install again this handler and if SIGINT is received
		// another time (thus getting it twice) we exit the whole program without
		// trying to delete the trace.
		if sig == syscall.SIGINT {
			sigIntReceivedNumber++

			if sigIntReceivedNumber > 1 {
				os.Exit(1)
			}

			sigHandler(traceID)
		}

		if *traceID != "" {
			DeleteTrace(*traceID)
		}
		if sig == syscall.SIGINT {
			os.Exit(0)
		} else {
			os.Exit(1)
		}
	}()
}

// PrintTraceOutputFromStream is used to print trace output using generic
// printing function.
// This function is must be used by trace which has TraceOutputMode set to
// Stream.
func PrintTraceOutputFromStream(traceID string, expectedState string, params *CommonFlags,
	transformLine func(string) string,
) error {
	traces, err := waitForTraceState(traceID, expectedState)
	if err != nil {
		return err
	}

	return genericStreamsDisplay(params, traces, transformLine)
}

// PrintTraceOutputFromStatus is used to print trace output using function
// pointer provided by caller.
// It will parse trace.Spec.Output and print it calling the function pointer.
func PrintTraceOutputFromStatus(traceID string, expectedState string, customResultsDisplay func(results []gadgetv1alpha1.Trace) error) error {
	traces, err := waitForTraceState(traceID, expectedState)
	if err != nil {
		return err
	}

	return customResultsDisplay(traces.Items)
}

// DeleteTrace deletes the traces for the given trace ID using RESTClient.
func DeleteTrace(traceID string) error {
	traceClient, err := getTraceClient()
	if err != nil {
		return err
	}

	deleteTraces(traceClient, traceID)

	return nil
}

// labelsFromFilter creates a string containing labels value from the given
// labelFilter.
func labelsFromFilter(filter map[string]string) string {
	labels := ""
	separator := ""

	// Loop on all fields of labelFilter.
	for labelName, labelValue := range filter {
		// If this field has no value, just skip it.
		if labelValue == "" {
			continue
		}

		// Concatenate the label to existing one.
		labels = fmt.Sprintf("%s%s%s=%v", labels, separator, labelName, labelValue)
		separator = ","
	}

	return labels
}

// getTraceListFromParameters returns traces associated with the given config.
func getTraceListFromParameters(config *TraceConfig) ([]gadgetv1alpha1.Trace, error) {
	filter := map[string]string{
		"gadgetName":    config.GadgetName,
		"nodeName":      config.CommonFlags.Node,
		"namespace":     config.CommonFlags.Namespace,
		"podName":       config.CommonFlags.Podname,
		"containerName": config.CommonFlags.Containername,
		"outputMode":    config.TraceOutputMode,
	}

	listTracesOptions := metav1.ListOptions{
		LabelSelector: labelsFromFilter(filter),
	}

	traces, err := getTraceListFromOptions(listTracesOptions)
	if err != nil {
		return []gadgetv1alpha1.Trace{}, err
	}

	return traces.Items, nil
}

// PrintAllTraces prints all traces corresponding to the given config.CommonFlags.
func PrintAllTraces(config *TraceConfig) error {
	traces, err := getTraceListFromParameters(config)
	if err != nil {
		return err
	}

	type printingInformation struct {
		namespace     string
		nodes         []string
		podname       string
		containerName string
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 4, ' ', 0)

	fmt.Fprintln(w, "NAMESPACE\tNODE(S)\tPOD\tCONTAINER\tTRACEID")

	printingMap := map[string]*printingInformation{}

	for _, trace := range traces {
		id, present := trace.ObjectMeta.Labels[GlobalTraceID]
		if !present {
			continue
		}

		node := trace.Spec.Node

		_, present = printingMap[id]
		if present {
			if node == "" {
				continue
			}

			// If an entry with this traceID already exists, we just update the node
			// name by concatenating it to the string.
			printingMap[id].nodes = append(printingMap[id].nodes, node)
		} else {
			// Otherwise, we simply create a new entry.
			if filter := trace.Spec.Filter; filter != nil {
				printingMap[id] = &printingInformation{
					namespace:     filter.Namespace,
					nodes:         []string{node},
					podname:       filter.Podname,
					containerName: filter.ContainerName,
				}
			} else {
				printingMap[id] = &printingInformation{
					nodes: []string{node},
				}
			}
		}
	}

	for id, info := range printingMap {
		sort.Strings(info.nodes)
		fmt.Fprintf(w, "%v\t%v\t%v\t%v\t%v\n", info.namespace, strings.Join(info.nodes, ","), info.podname, info.containerName, id)
	}

	w.Flush()

	return nil
}

// RunTraceAndPrintStream creates a trace, prints its output and deletes
// it.
// It equals calling separately CreateTrace(), then PrintTraceOutputFromStream()
// and DeleteTrace().
// This function is thought to be used with "one-run" gadget, i.e. gadget
// which runs a trace when it is created.
func RunTraceAndPrintStream(config *TraceConfig, transformLine func(string) string) error {
	var traceID string

	sigHandler(&traceID)

	if config.TraceOutputMode != "Stream" {
		return errors.New("TraceOutputMode must be Stream. Otherwise, call RunTraceAndPrintStatusOutput")
	}

	traceID, err := CreateTrace(config)
	if err != nil {
		return fmt.Errorf("error creating trace: %w", err)
	}

	defer DeleteTrace(traceID)

	return PrintTraceOutputFromStream(traceID, config.TraceOutputState, config.CommonFlags, transformLine)
}

// RunTraceStreamCallback creates a stream trace and calls callback each
// time one of the tracers produces a new line on any of the nodes.
func RunTraceStreamCallback(config *TraceConfig, callback func(line string, node string)) error {
	var traceID string

	sigHandler(&traceID)

	if config.TraceOutputMode != "Stream" {
		return errors.New("TraceOutputMode must be Stream")
	}

	traceID, err := CreateTrace(config)
	if err != nil {
		return fmt.Errorf("error creating trace: %w", err)
	}

	defer DeleteTrace(traceID)

	traces, err := waitForTraceState(traceID, config.TraceOutputState)
	if err != nil {
		return err
	}

	return genericStreams(config.CommonFlags, traces, callback, nil)
}

// RunTraceAndPrintStatusOutput creates a trace, prints its output and deletes
// it.
// It equals calling separately CreateTrace(), then PrintTraceOutputFromStatus()
// and DeleteTrace().
// This function is thought to be used with "one-run" gadget, i.e. gadget
// which runs a trace when it is created.
func RunTraceAndPrintStatusOutput(config *TraceConfig, customResultsDisplay func(results []gadgetv1alpha1.Trace) error) error {
	var traceID string

	sigHandler(&traceID)

	if config.TraceOutputMode == "Stream" {
		return errors.New("TraceOutputMode must not be Stream. Otherwise, call RunTraceAndPrintStream")
	}

	traceID, err := CreateTrace(config)
	if err != nil {
		return fmt.Errorf("error creating trace: %w", err)
	}

	defer DeleteTrace(traceID)

	return PrintTraceOutputFromStatus(traceID, config.TraceOutputState, customResultsDisplay)
}

func genericStreamsDisplay(
	params *CommonFlags,
	results *gadgetv1alpha1.TraceList,
	transformLine func(string) string,
) error {
	transform := func(line string) string {
		if params.OutputMode == OutputModeJSON {
			return line
		}
		return transformLine(line)
	}

	return genericStreams(params, results, nil, transform)
}

func getTraceStream(
	podname string,
	trace gadgetv1alpha1.Trace,
	transform func(line string) string,
) error {
	// setup port forwarding
	stopCh := make(chan struct{}, 1)
	readyCh := make(chan struct{})

	config, err := KubernetesConfigFlags.ToRESTConfig()
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward",
		"gadget", podname)
	hostIP := strings.TrimLeft(config.Host, "https:/")

	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		return fmt.Errorf("failed to create rount tripper: %w", err)
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost,
		&url.URL{Scheme: "https", Path: path, Host: hostIP})
	fw, err := portforward.New(dialer, []string{"0:7500"}, stopCh, readyCh, nil, nil)
	if err != nil {
		return fmt.Errorf("failed to create port forwarding: %w", err)
	}

	defer close(stopCh)

	go func() {
		fw.ForwardPorts()
	}()

	<-readyCh

	ports, err := fw.GetPorts()
	if err != nil {
		return fmt.Errorf("failed to get ports: %w", err)
	}

	if len(ports) != 1 {
		return fmt.Errorf("one port expected. Found %d", len(ports))
	}

	// run grpc
	conn, err := grpc.Dial(fmt.Sprintf("localhost:%d", ports[0].Local), grpc.WithInsecure())
	if err != nil {
		return fmt.Errorf("fail to dial: %w", err)
	}
	defer conn.Close()
	client := pb.NewGadgetTracerManagerClient(conn)

	namespace := trace.ObjectMeta.Namespace
	name := trace.ObjectMeta.Name

	stream, err := client.ReceiveStream(context.Background(), &pb.TracerID{
		Id: fmt.Sprintf("trace_%s_%s", namespace, name),
	})
	if err != nil {
		return fmt.Errorf("failed to receive stream: %w", err)
	}

	for {
		line, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("error reading stream: %w", err)
		}

		fmt.Println(transform(line.Line))
	}

	return nil
}

func genericStreams(
	params *CommonFlags,
	results *gadgetv1alpha1.TraceList,
	callback func(line string, node string),
	transform func(line string) string,
) error {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	client, err := k8sutil.NewClientsetFromConfigFlags(KubernetesConfigFlags)
	if err != nil {
		return WrapInErrSetupK8sClient(err)
	}

	podsByNode := map[string]string{}

	pods, err := client.CoreV1().Pods("gadget").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, pod := range pods.Items {
		podsByNode[pod.Spec.NodeName] = pod.Name
	}

	for _, trace := range results.Items {
		if params.Node != "" && trace.Spec.Node != params.Node {
			continue
		}

		pod, ok := podsByNode[trace.Spec.Node]
		if !ok {
			continue
		}

		mytrace := trace

		go func() {
			err := getTraceStream(pod, mytrace, transform)
			if err != nil {
				fmt.Printf("error was %s\n", err)
			}
		}()

	}

	<-sigs

	return nil
}

// DeleteTracesByGadgetName removes all traces with this gadget name
func DeleteTracesByGadgetName(gadget string) error {
	traceClient, err := getTraceClient()
	if err != nil {
		return err
	}

	listTracesOptions := metav1.ListOptions{
		LabelSelector: fmt.Sprintf("gadgetName=%s", gadget),
	}

	return traceClient.GadgetV1alpha1().Traces("gadget").DeleteCollection(
		context.TODO(), metav1.DeleteOptions{}, listTracesOptions,
	)
}

func ListTracesByGadgetName(gadget string) ([]gadgetv1alpha1.Trace, error) {
	listTracesOptions := metav1.ListOptions{
		LabelSelector: fmt.Sprintf("gadgetName=%s", gadget),
	}

	traces, err := getTraceListFromOptions(listTracesOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to get traces by gadget name: %w", err)
	}

	return traces.Items, nil
}
